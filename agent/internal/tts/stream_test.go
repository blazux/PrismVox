package tts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"phoneagent/internal/voice"
	"reflect"
	"testing"
	"time"
)

func TestPCMStreamsBeforeProviderFinishesAndPreservesSplitSamples(t *testing.T) {
	release := make(chan struct{})
	early := make(chan []int16, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/text-to-speech/fixture/stream" || r.URL.Query().Get("output_format") != "pcm_8000" || r.Header.Get("xi-api-key") != "fixture-key" {
			t.Error("incorrect provider contract")
		}
		w.Header().Set("Content-Type", "audio/pcm")
		w.Write([]byte{0x34})
		w.(http.Flusher).Flush()
		w.Write([]byte{0x12, 0xff})
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		w.Write([]byte{0xff})
	}))
	defer server.Close()
	c := New()
	c.Configure(voice.Settings{TTSProvider: "elevenlabs", BaseURL: server.URL, VoiceID: "fixture", APIKey: "fixture-key", TTSModel: "fixture-model"})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- c.Stream(ctx, "Hello", func(pcm []int16) error { early <- pcm; return nil }) }()
	select {
	case pcm := <-early:
		if len(pcm) != 1 || pcm[0] != 0x1234 {
			t.Fatalf("bad first sample: %v", pcm)
		}
	case <-ctx.Done():
		close(release)
		t.Fatal("audio waited for complete provider response")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if pcm := <-early; len(pcm) != 1 || pcm[0] != -1 {
		t.Fatalf("lost split sample: %v", pcm)
	}
}

func TestStreamCancellationClosesProviderRequest(t *testing.T) {
	started, stopped := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(stopped)
	}))
	defer server.Close()
	c := New()
	c.Configure(voice.Settings{TTSProvider: "elevenlabs", BaseURL: server.URL, VoiceID: "fixture"})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Stream(ctx, "Hello", func([]int16) error { return nil }) }()
	<-started
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancellation succeeded silently")
		}
	case <-time.After(time.Second):
		t.Fatal("cancel did not release synthesis")
	}
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("provider request survived cancellation")
	}
}

func TestStreamStopsWhenConsumerRejectsAudio(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte{1, 0}) }))
	defer server.Close()
	c := New()
	c.Configure(voice.Settings{TTSProvider: "elevenlabs", BaseURL: server.URL, VoiceID: "fixture"})
	stop := errors.New("stop playback")
	if err := c.Stream(context.Background(), "Hello", func([]int16) error { return stop }); !errors.Is(err, stop) {
		t.Fatal(err)
	}
}

func TestFrenchDictionary(t *testing.T) {
	c := New()
	c.Configure(voice.Settings{Language: "fr"})
	c.SpellWords = "VoIP"
	c.SpellExceptions = "OK NASA"
	if got := c.prepareText("VoIP et IP OK NASA"); got != "vé o i pé et i pé OK NASA" {
		t.Fatal(got)
	}
}

func TestAdaptiveSpeechDoesNotForceTheDefaultLanguage(t *testing.T) {
	var languages []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Language string `json:"language_code"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		languages = append(languages, body.Language)
		w.Write([]byte{1, 0})
	}))
	defer server.Close()
	c := New()
	s := voice.Settings{TTSProvider: "elevenlabs", BaseURL: server.URL, VoiceID: "fixture", Language: "en", AutoLanguage: true}
	c.Configure(s)
	c.SpellWords = "VoIP"
	english := "Please check the VoIP connection before calling again."
	if c.prepareText(english) != english {
		t.Fatal("French letter names inserted into English speech")
	}
	if err := c.Stream(context.Background(), english, func([]int16) error { return nil }); err != nil {
		t.Fatal(err)
	}
	s.AutoLanguage = false
	c.Configure(s)
	if err := c.Stream(context.Background(), english, func([]int16) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(languages, []string{"", "en"}) {
		t.Fatalf("adaptive/fixed language contract: %v", languages)
	}
}

func TestSpeechContinuityIsBoundedAndPrivateToCall(t *testing.T) {
	var bodies []struct {
		Previous []string           `json:"previous_request_ids"`
		Settings map[string]float64 `json:"voice_settings"`
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Previous []string           `json:"previous_request_ids"`
			Settings map[string]float64 `json:"voice_settings"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		bodies = append(bodies, body)
		w.Header().Set("request-id", fmt.Sprintf("request-%d", len(bodies)))
		w.Write([]byte{1, 0})
	}))
	defer server.Close()
	s := voice.Settings{TTSProvider: "elevenlabs", BaseURL: server.URL, VoiceID: "fixture", TTSModel: "eleven_flash_v2_5"}
	stability := 0.0
	s.Stability = &stability
	c := New()
	c.Configure(s)
	speak := func(c *Client) {
		t.Helper()
		if err := c.Stream(context.Background(), "Une phrase.", func([]int16) error { return nil }); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 5; i++ {
		speak(c)
	}
	if len(bodies[0].Previous) != 0 || !reflect.DeepEqual(bodies[4].Previous, []string{"request-2", "request-3", "request-4"}) {
		t.Fatalf("incorrect continuity: %+v", bodies)
	}
	if v, ok := bodies[0].Settings["stability"]; !ok || v != 0 {
		t.Fatal("zero stability override lost")
	}
	other := New()
	s.Stability = nil
	other.Configure(s)
	speak(other)
	if len(bodies[5].Previous) != 0 || bodies[5].Settings != nil {
		t.Fatal("call context or settings leaked")
	}
	c.Configure(s)
	speak(c)
	if len(bodies[6].Previous) != 0 {
		t.Fatal("reconfiguration reused another voice's context")
	}
	s.TTSModel = "eleven_v3"
	c.Configure(s)
	speak(c)
	speak(c)
	if len(bodies[8].Previous) != 0 {
		t.Fatal("v3 received unsupported stitching")
	}
}

func TestInterruptedSpeechDoesNotEnterContinuity(t *testing.T) {
	var previous []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Previous []string `json:"previous_request_ids"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		previous = body.Previous
		w.Header().Set("request-id", "interrupted")
		w.Write([]byte{1, 0})
	}))
	defer server.Close()
	c := New()
	c.Configure(voice.Settings{TTSProvider: "elevenlabs", BaseURL: server.URL, VoiceID: "fixture"})
	c.continuity = []speechRequest{{id: "expired", at: time.Now().Add(-3 * time.Hour)}}
	stop := errors.New("caller interrupted")
	if err := c.Stream(context.Background(), "Stop.", func([]int16) error { return stop }); !errors.Is(err, stop) {
		t.Fatal(err)
	}
	if err := c.Stream(context.Background(), "Reprenons.", func([]int16) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if len(previous) != 0 {
		t.Fatalf("interrupted or expired audio reused: %v", previous)
	}
}
