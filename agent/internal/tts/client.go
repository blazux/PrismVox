// Package tts provides a client for the TTS HTTP service.
package tts

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"phoneagent/internal/audio"
	"phoneagent/internal/voice"
	"strings"
	"sync"
	"time"
)

// Client calls the TTS service.
type Client struct {
	settings     voice.Settings
	URL          string
	http         *http.Client
	continuityMu sync.Mutex
	continuity   []speechRequest
	cachedSpeech string
	cacheDir     string
	// Acronym dictionary (DB-managed, set per call). Empty → the service uses its
	// env defaults. SpellWords = always spell; SpellExceptions = read as words.
	SpellWords      string
	SpellExceptions string
}

// Fork shares provider and cache settings, but never speech continuity.
// Preparing an unplayed phrase must not become previous spoken text.
func (c *Client) Fork() *Client {
	return &Client{settings: c.settings, URL: c.URL, http: c.http, cacheDir: c.cacheDir, SpellWords: c.SpellWords, SpellExceptions: c.SpellExceptions}
}

// New creates a client for the configured speech service.
func New() *Client {
	u := os.Getenv("TTS_URL")
	if u == "" {
		u = "http://tts:8000/synthesize"
	}
	cacheDir := os.Getenv("TTS_CACHE_DIR")
	if cacheDir == "" {
		cacheDir = "/var/cache/prismvox/tts"
	} else if cacheDir == "off" {
		cacheDir = ""
	}
	return &Client{
		URL:      u,
		http:     &http.Client{Timeout: 30 * time.Second},
		cacheDir: cacheDir,
	}
}

// Synthesize is the batch compatibility entry point. Callers with a lifecycle
// should use SynthesizeContext so interruption stops the provider request.
func (c *Client) Synthesize(text string) ([]byte, error) {
	return c.SynthesizeContext(context.Background(), text)
}

func (c *Client) SynthesizeContext(ctx context.Context, text string) ([]byte, error) {
	if c.settings.TTSProvider == "elevenlabs" {
		var pcm []int16
		if err := c.Stream(ctx, text, func(samples []int16) error { pcm = append(pcm, samples...); return nil }); err != nil {
			return nil, err
		}
		return audio.EncodeWAV(audio.Int16ToFloat32(pcm), 8000)
	}
	if strings.TrimSpace(text) == "" {
		return nil, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	path := c.cachePath(text, "wav")
	if path == "" {
		return c.synthesizeWAV(ctx, text)
	}
	unlock, err := lockAudio(ctx, path)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if data, ok := readAudio(path); ok {
		return data, nil
	}
	data, err := c.synthesizeWAV(ctx, text)
	if err == nil && ctx.Err() == nil {
		if samples, _, decodeErr := audio.DecodeWAV(data); decodeErr == nil && len(samples) > 0 {
			writeAudio(path, data)
		}
	}
	return data, err
}

func (c *Client) synthesizeWAV(ctx context.Context, text string) ([]byte, error) {

	body := map[string]any{"text": text}
	endpoint := c.URL
	if c.settings.TTSProvider == "openai" {
		body = map[string]any{"input": c.prepareText(text), "model": c.settings.ServiceTTSModel, "response_format": "wav"}
		if c.settings.ServiceVoice != "" {
			body["voice"] = c.settings.ServiceVoice
		}
		endpoint = strings.TrimRight(endpoint, "/") + "/audio/speech"
	}
	if c.SpellWords != "" && c.settings.TTSProvider != "openai" {
		body["spell_words"] = c.SpellWords
	}
	if c.SpellExceptions != "" && c.settings.TTSProvider != "openai" {
		body["spell_exceptions"] = c.SpellExceptions
	}
	b, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}

	data, err := c.synthesizeAt(ctx, endpoint, b)
	if err != nil {
		log.Printf("tts: synthesis failed")
	}
	return data, err
}

func (c *Client) synthesizeAt(ctx context.Context, url string, jsonBody []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(jsonBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.settings.TTSProvider == "openai" && c.settings.ServiceAPIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.settings.ServiceAPIKey)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		log.Printf("tts: error: %v", err)
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("tts: HTTP %d", resp.StatusCode)
	}

	return io.ReadAll(io.LimitReader(resp.Body, 32<<20))
}
