package stt

import (
	"context"
	"encoding/json"
	"github.com/gorilla/websocket"
	"net/http"
	"net/http/httptest"
	"phoneagent/internal/voice"
	"testing"
	"time"
)

func TestBatchRecognitionLanguageMode(t *testing.T) {
	text := "Je voudrais prendre un rendez-vous demain matin, s'il vous plaît."
	var languages []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Error(err)
		}
		defer r.MultipartForm.RemoveAll()
		languages = append(languages, r.FormValue("language_code"))
		json.NewEncoder(w).Encode(map[string]string{"text": text, "language_code": "fra"})
	}))
	defer server.Close()
	c := New()
	s := voice.Settings{STTProvider: "elevenlabs", STTModel: "scribe_v2", BaseURL: server.URL, Language: "en", AutoLanguage: true}
	c.Configure(s)
	observed := ""
	c.OnLanguage = func(code, content string) { observed = code }
	if got, err := c.Transcribe(context.Background(), []byte("fixture")); err != nil || got != text || observed != "fr" {
		t.Fatalf("adaptive batch transcription: %q %q %v", got, observed, err)
	}
	s.AutoLanguage = false
	c.Configure(s)
	if _, err := c.Transcribe(context.Background(), []byte("fixture")); err != nil {
		t.Fatal(err)
	}
	if len(languages) != 2 || languages[0] != "" || languages[1] != "en" {
		t.Fatalf("adaptive/fixed recognition contract: %v", languages)
	}
}

func TestRealtimeLanguageMetadataDoesNotDelayTranscript(t *testing.T) {
	release := make(chan struct{})
	observed := make(chan string, 4)
	text := "Je voudrais prendre un rendez-vous demain matin, s'il vous plaît."
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("language_code") != "" || r.URL.Query().Get("include_language_detection") != "true" {
			t.Error("automatic recognition is pinned")
		}
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			var input struct {
				Commit bool `json:"commit"`
			}
			if conn.ReadJSON(&input) != nil {
				return
			}
			if input.Commit {
				break
			}
		}
		conn.WriteJSON(map[string]string{"message_type": "committed_transcript", "text": text})
		<-release
		conn.WriteJSON(map[string]string{"message_type": "committed_transcript_with_timestamps", "text": text, "language_code": "fr"})
	}))
	defer server.Close()
	c := New()
	c.Configure(voice.Settings{STTProvider: "elevenlabs", STTModel: "scribe_v2_realtime", BaseURL: server.URL, Language: "en", AutoLanguage: true})
	c.OnLanguage = func(code, content string) {
		if content != text {
			t.Error("metadata text changed")
		}
		observed <- code
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	stream := c.Begin(ctx)
	defer stream.Close()
	stream.Add(make([]int16, 1600))
	start := time.Now()
	got, err := stream.Finish()
	if err != nil || got != text {
		close(release)
		t.Fatalf("transcript: %q %v", got, err)
	}
	if time.Since(start) > 200*time.Millisecond {
		close(release)
		t.Fatal("transcription waited for optional language metadata")
	}
	// One local text hint, then the delayed provider hint after Finish returned.
	<-observed
	close(release)
	select {
	case code := <-observed:
		if code != "fr" {
			t.Fatal(code)
		}
	case <-ctx.Done():
		t.Fatal("delayed metadata was lost")
	}
}
