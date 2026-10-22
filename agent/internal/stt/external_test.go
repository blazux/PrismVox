package stt

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"phoneagent/internal/voice"
	"testing"
)

func TestGenericTranscriptionAPI(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/audio/transcriptions" || r.Header.Get("Authorization") != "Bearer service-fixture" || r.Header.Get("xi-api-key") != "" {
			t.Error("incorrect API endpoint or credential scope")
		}
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Error(err)
			return
		}
		file, _, err := r.FormFile("file")
		if err != nil {
			t.Error(err)
			return
		}
		defer file.Close()
		b, _ := io.ReadAll(file)
		if string(b) != "fixture-audio" || r.FormValue("model") != "custom-stt" || r.FormValue("language") != "fr" || r.FormValue("prompt") != "Example" {
			t.Error("incorrect transcription fields")
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"text":" Bonjour. "}`)
	}))
	defer server.Close()
	c := New()
	c.Configure(voice.Settings{STTProvider: "openai", STTURL: server.URL + "/v1", ServiceSTTModel: "custom-stt", ServiceAPIKey: "service-fixture", APIKey: "never-send-eleven-key", Language: "fr"})
	c.Hotwords = "Example"
	text, err := c.Transcribe(context.Background(), []byte("fixture-audio"))
	if err != nil || text != "Bonjour." {
		t.Fatalf("%q %v", text, err)
	}
}
