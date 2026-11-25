package stt

import (
	"context"
	"net/http"
	"net/http/httptest"
	"phoneagent/internal/voice"
	"sync/atomic"
	"testing"
)

func TestProviderRedirectCannotForwardKeyOrCallerAudio(t *testing.T) {
	var leaked atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leaked.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"text":"synthetic transcript"}`))
	}))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer source.Close()
	c := New()
	c.Configure(voice.Settings{STTProvider: "elevenlabs", BaseURL: source.URL, APIKey: "fixture-private-key", STTModel: "scribe_v2"})
	_, err := c.Transcribe(context.Background(), []byte("synthetic fixture audio"))
	if err == nil || leaked.Load() != 0 {
		t.Fatalf("provider redirect accepted: error=%v forwardedRequests=%d", err, leaked.Load())
	}
}
