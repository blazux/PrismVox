package stt

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/gorilla/websocket"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"phoneagent/internal/audio"
	"phoneagent/internal/voice"
	"strings"
	"time"
)

func (c *Client) Configure(s voice.Settings) {
	c.settings = s
	if s.STTURL != "" {
		c.url = strings.TrimRight(s.STTURL, "/")
	}
}
func (c *Client) Realtime() bool {
	return c.settings.STTProvider == "elevenlabs" && c.settings.STTModel == "scribe_v2_realtime"
}

type streamResult struct {
	text string
	err  error
}

// Stream owns one utterance. It starts only once local VAD detects speech, so
// waiting callers do not consume a provider stream. Add never blocks RTP capture;
// an overloaded connection fails explicitly rather than silently losing words.
type Stream struct {
	input  chan []int16
	result chan streamResult
	ctx    context.Context
	cancel context.CancelFunc
	failed bool
}

func (c *Client) Begin(ctx context.Context) *Stream {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	s := &Stream{input: make(chan []int16, 128), result: make(chan streamResult, 1), ctx: ctx, cancel: cancel}
	go func() { text, err := c.stream(ctx, s.input); s.result <- streamResult{text, err}; cancel() }()
	return s
}

// Add and Finish belong to the capture goroutine, not multiple producers.
func (s *Stream) Add(pcm []int16) {
	if s.failed {
		return
	}
	select {
	case s.input <- append([]int16(nil), pcm...):
	case <-s.ctx.Done():
		s.failed = true
	default:
		s.failed = true
		s.cancel()
	}
}
func (s *Stream) Close() { s.cancel() }
func (s *Stream) Finish() (string, error) {
	defer s.cancel()
	close(s.input)
	select {
	case r := <-s.result:
		return r.text, r.err
	case <-s.ctx.Done():
		select {
		case r := <-s.result:
			return r.text, r.err
		default:
			return "", s.ctx.Err()
		}
	}
}
func (c *Client) stream(ctx context.Context, input <-chan []int16) (string, error) {
	u, err := url.Parse(c.settings.BaseURL + "/speech-to-text/realtime")
	if err != nil {
		return "", errors.New("invalid STT provider URL")
	}
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	case "http":
		u.Scheme = "ws"
	default:
		return "", errors.New("invalid STT provider URL")
	}
	q := u.Query()
	q.Set("model_id", "scribe_v2_realtime")
	q.Set("audio_format", "pcm_16000")
	q.Set("commit_strategy", "manual")
	if c.settings.Language != "" {
		q.Set("language_code", c.settings.Language)
	}
	u.RawQuery = q.Encode()
	dial := websocket.Dialer{HandshakeTimeout: 5 * time.Second, Proxy: http.ProxyFromEnvironment}
	conn, resp, err := dial.DialContext(ctx, u.String(), http.Header{"Xi-Api-Key": {c.settings.APIKey}})
	if resp != nil && resp.Body != nil {
		defer resp.Body.Close()
	}
	if err != nil {
		return "", errors.New("STT realtime connection failed")
	}
	defer conn.Close()
	conn.SetReadLimit(1 << 20)
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			conn.Close()
		case <-done:
		}
	}()
	results := make(chan streamResult, 1)
	go func() {
		for {
			var ev struct {
				Type  string `json:"message_type"`
				Text  string `json:"text"`
				Error string `json:"error"`
			}
			if err := conn.ReadJSON(&ev); err != nil {
				results <- streamResult{err: errors.New("STT realtime stream interrupted")}
				return
			}
			if ev.Error != "" {
				results <- streamResult{err: errors.New("STT realtime provider rejected the request")}
				return
			}
			switch ev.Type {
			case "committed_transcript":
				results <- streamResult{text: strings.TrimSpace(ev.Text)}
				return
			case "session_started", "partial_transcript", "warning":
			default:
				if strings.Contains(ev.Type, "error") || ev.Type == "rate_limited" || ev.Type == "auth_error" || ev.Type == "quota_exceeded" {
					results <- streamResult{err: fmt.Errorf("STT realtime provider rejected the request")}
					return
				}
			}
		}
	}()
	send := func(pcm []int16, commit bool) error {
		b := make([]byte, len(pcm)*2)
		for i, v := range pcm {
			binary.LittleEndian.PutUint16(b[2*i:], uint16(v))
		}
		conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		return conn.WriteJSON(map[string]interface{}{"message_type": "input_audio_chunk", "audio_base_64": base64.StdEncoding.EncodeToString(b), "commit": commit, "sample_rate": 16000})
	}
	var pending []int16
	for {
		select {
		case pcm, ok := <-input:
			if !ok {
				// Keep the last audio in the commit message (no duplicate transcription).
				if len(pending) == 0 {
					pending = make([]int16, 1600)
				}
				if err := send(pending, true); err != nil {
					return "", errors.New("STT realtime upload interrupted")
				}
				select {
				case <-time.After(15 * time.Second):
					return "", errors.New("STT realtime final transcript timed out")
				case result := <-results:
					return result.text, result.err
				case <-ctx.Done():
					return "", ctx.Err()
				}
			}
			pending = append(pending, pcm...)
			// 100ms chunks. Hold a chunk for the final explicit commit.
			for len(pending) > 1600 {
				if err := send(pending[:1600], false); err != nil {
					return "", errors.New("STT realtime upload interrupted")
				}
				pending = pending[1600:]
			}
		case result := <-results:
			return result.text, result.err
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
}

func (c *Client) transcribeEleven(ctx context.Context, wav []byte) (string, error) {
	if c.Realtime() {
		samples, rate, err := audio.DecodeWAV(wav)
		if err != nil {
			return "", err
		}
		pcm := audio.Float32ToInt16(audio.Resample(samples, rate, 16000))
		s := c.Begin(ctx)
		defer s.Close()
		// Batch callers (e.g. barge-in validation) still use bounded chunk writes.
		go func() {
			for len(pcm) > 0 {
				n := 1600
				if len(pcm) < n {
					n = len(pcm)
				}
				select {
				case s.input <- pcm[:n]:
					pcm = pcm[n:]
				case <-s.ctx.Done():
					return
				}
			}
			close(s.input)
		}()
		select {
		case r := <-s.result:
			return r.text, r.err
		case <-s.ctx.Done():
			select {
			case r := <-s.result:
				return r.text, r.err
			default:
				return "", s.ctx.Err()
			}
		}
	}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	_ = writer.WriteField("model_id", c.settings.STTModel)
	if c.settings.Language != "" {
		_ = writer.WriteField("language_code", c.settings.Language)
	}
	f, err := writer.CreateFormFile("file", "audio.wav")
	if err != nil {
		return "", err
	}
	_, _ = f.Write(wav)
	_ = writer.Close()
	req, err := http.NewRequestWithContext(ctx, "POST", c.settings.BaseURL+"/speech-to-text", &body)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("xi-api-key", c.settings.APIKey)
	resp, err := c.http.Do(req)
	if err != nil {
		return "", errors.New("STT provider request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("ElevenLabs STT HTTP %d", resp.StatusCode)
	}
	var result struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&result); err != nil {
		return "", errors.New("invalid STT provider response")
	}
	return strings.TrimSpace(result.Text), nil
}
