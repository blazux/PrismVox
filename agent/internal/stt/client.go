// Package stt provides clients for configured speech recognition APIs.
package stt

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"phoneagent/internal/locale"
	"phoneagent/internal/voice"
	"strings"
	"time"
	"unicode"
)

// Client calls the STT transcription service.
type Client struct {
	settings   voice.Settings
	Hotwords   string
	url        string
	http       *http.Client
	OnLanguage func(string, string)
}

func (c *Client) observeLanguage(code, text string) {
	if c.OnLanguage == nil {
		return
	}
	letters := 0
	for _, r := range text {
		if unicode.IsLetter(r) {
			letters++
		}
	}
	if letters < 12 {
		return
	}
	if code = locale.Normalize(code); code == "" {
		code = locale.Detect(text)
	}
	if code != "" {
		c.OnLanguage(code, strings.TrimSpace(text))
	}
}

// Side legs and echo probes must not change the caller's language.
func (c *Client) Clone() *Client { copy := *c; copy.OnLanguage = nil; return &copy }

// New creates a client for an optional local or external speech service.
func New() *Client {
	u := os.Getenv("STT_URL")
	if u == "" {
		u = "http://stt:9000"
	}
	return &Client{
		url:  strings.TrimRight(u, "/"),
		http: &http.Client{Timeout: 30 * time.Second},
	}
}

// Transcribe sends wavBytes (16kHz mono PCM_16 WAV) to the STT service and returns
// the recognised text.
func (c *Client) Transcribe(ctx context.Context, wavBytes []byte) (string, error) {
	if c.settings.STTProvider == "elevenlabs" {
		return c.transcribeEleven(ctx, wavBytes)
	}
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)

	field := "audio"
	endpoint := c.url + "/transcribe"
	if c.settings.STTProvider == "openai" {
		field = "file"
		endpoint = c.url + "/audio/transcriptions"
	}
	fw, err := mw.CreateFormFile(field, "audio.wav")
	if err != nil {
		return "", err
	}
	if _, err := fw.Write(wavBytes); err != nil {
		return "", err
	}
	if c.settings.STTProvider == "openai" {
		if c.settings.ServiceSTTModel != "" {
			_ = mw.WriteField("model", c.settings.ServiceSTTModel)
		}
		_ = mw.WriteField("response_format", "json")
		if c.settings.Language != "" && !c.settings.AutoLanguage {
			_ = mw.WriteField("language", c.settings.Language)
		}
		if c.Hotwords != "" {
			_ = mw.WriteField("prompt", c.Hotwords)
		}
	} else if c.Hotwords != "" {
		_ = mw.WriteField("hotwords", c.Hotwords)
	}
	mw.Close()

	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, &buf)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if c.settings.STTProvider == "openai" && c.settings.ServiceAPIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.settings.ServiceAPIKey)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("stt HTTP %d", resp.StatusCode)
	}

	var result struct {
		Text     string `json:"text"`
		Language string `json:"language"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return "", fmt.Errorf("stt decode: %w", err)
	}
	c.observeLanguage(result.Language, result.Text)
	return strings.TrimSpace(result.Text), nil
}
