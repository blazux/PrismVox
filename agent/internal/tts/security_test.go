package tts

import (
	"context"
	"net/http"
	"net/http/httptest"
	"phoneagent/internal/voice"
	"sync/atomic"
	"testing"
)

func TestProviderRedirectCannotForwardKeyOrSpokenText(t *testing.T) {
	var leaked atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leaked.Add(1)
		w.Write([]byte{0, 0})
	}))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer source.Close()
	t.Setenv("TTS_CACHE_DIR", "off")
	c := New()
	c.Configure(voice.Settings{TTSProvider: "elevenlabs", BaseURL: source.URL, APIKey: "fixture-private-key", VoiceID: "fixture", TTSModel: "fixture"})
	err := c.Stream(context.Background(), "Private synthetic sentence", func([]int16) error { return nil })
	if err == nil || leaked.Load() != 0 {
		t.Fatalf("provider redirect accepted: error=%v forwardedRequests=%d", err, leaked.Load())
	}
}
