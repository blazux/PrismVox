package tts

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"phoneagent/internal/audio"
	"phoneagent/internal/voice"
	"strings"
	"time"
)

func (c *Client) Configure(s voice.Settings) {
	c.continuityMu.Lock()
	c.continuity = nil
	c.cachedSpeech = ""
	c.continuityMu.Unlock()
	c.settings = s
	if s.TTSURL != "" {
		c.URL = s.TTSURL
	}
}

// Stream emits mono PCM16 samples at 8 kHz, matching Asterisk's outbound RTP.
// Service providers retain their batch WAV contract; ElevenLabs streams binary
// PCM without waiting for the complete sentence. emit may apply backpressure.
func (c *Client) Stream(ctx context.Context, text string, emit func([]int16) error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if strings.TrimSpace(text) == "" {
		return nil
	}
	if c.settings.TTSProvider != "elevenlabs" {
		wav, err := c.SynthesizeContext(ctx, text)
		if err != nil {
			return err
		}
		samples, rate, err := audio.DecodeWAV(wav)
		if err != nil {
			return err
		}
		return emit(audio.Float32ToInt16(audio.Resample(samples, rate, 8000)))
	}
	return c.streamCached(ctx, text, emit)
}

func (c *Client) streamElevenLabs(ctx context.Context, text string, emit func([]int16) error) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	body := map[string]any{"text": c.prepareText(text), "model_id": c.settings.TTSModel}
	if c.settings.Stability != nil {
		body["voice_settings"] = map[string]any{"stability": *c.settings.Stability}
	}
	// v3 does not support request stitching. Keep context private to this call's
	// client, and only reuse requests whose audio was completely consumed.
	stitch := !strings.HasPrefix(c.settings.TTSModel, "eleven_v3")
	if stitch {
		if previous := c.previousCachedSpeech(); previous != "" {
			body["previous_text"] = previous
		} else if ids := c.previousSpeechRequests(); len(ids) > 0 {
			body["previous_request_ids"] = ids
		}
	}
	if c.settings.Language != "" && !c.settings.AutoLanguage {
		body["language_code"] = c.settings.Language
	}
	payload, _ := json.Marshal(body)
	endpoint := c.settings.BaseURL + "/text-to-speech/" + url.PathEscape(c.settings.VoiceID) + "/stream?output_format=pcm_8000"
	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("xi-api-key", c.settings.APIKey)
	streamHTTP := *c.http
	streamHTTP.Timeout = 0
	resp, err := streamHTTP.Do(req)
	if err != nil {
		return fmt.Errorf("TTS provider request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("ElevenLabs TTS HTTP %d", resp.StatusCode)
	}
	// A provider can split a PCM sample between network reads. Preserve the odd byte.
	buf := make([]byte, 1600)
	var tail []byte
	received := false
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			data := append(tail, buf[:n]...)
			even := len(data) &^ 1
			samples := make([]int16, even/2)
			for i := range samples {
				samples[i] = int16(binary.LittleEndian.Uint16(data[2*i:]))
			}
			tail = append([]byte(nil), data[even:]...)
			if len(samples) > 0 {
				received = true
				if err := emit(samples); err != nil {
					return err
				}
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return fmt.Errorf("TTS audio stream interrupted")
		}
	}
	if len(tail) != 0 {
		return fmt.Errorf("TTS returned an incomplete PCM sample")
	}
	if !received {
		return fmt.Errorf("TTS returned no audio")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if stitch {
		c.rememberSpeechRequest(resp.Header.Get("request-id"))
	}
	return nil
}

type speechRequest struct {
	id string
	at time.Time
}

func (c *Client) previousSpeechRequests() []string {
	c.continuityMu.Lock()
	defer c.continuityMu.Unlock()
	var ids []string
	for _, r := range c.continuity {
		if time.Since(r.at) < 2*time.Hour {
			ids = append(ids, r.id)
		}
	}
	return ids
}

func (c *Client) rememberSpeechRequest(id string) {
	c.continuityMu.Lock()
	defer c.continuityMu.Unlock()
	c.cachedSpeech = ""
	if id == "" {
		c.continuity = nil
		return
	}
	c.continuity = append(c.continuity, speechRequest{id: id, at: time.Now()})
	if len(c.continuity) > 3 {
		c.continuity = c.continuity[len(c.continuity)-3:]
	}
}

func (c *Client) previousCachedSpeech() string {
	c.continuityMu.Lock()
	defer c.continuityMu.Unlock()
	return c.cachedSpeech
}

func (c *Client) rememberCachedSpeech(text string) {
	c.continuityMu.Lock()
	defer c.continuityMu.Unlock()
	// Only the locally spoken text follows this call, never another call's
	// provider request IDs. Bound the context sent with the next fresh synthesis.
	c.continuity = nil
	runes := []rune(text)
	if len(runes) > 1000 {
		runes = runes[len(runes)-1000:]
	}
	c.cachedSpeech = string(runes)
}
