package tts

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"phoneagent/internal/audio"
	"phoneagent/internal/voice"
	"testing"
)

func TestGenericSpeechAPI(t *testing.T) {
	wav, _ := audio.EncodeWAV(make([]float32, 1600), 16000)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/audio/speech" || r.Header.Get("Authorization") != "Bearer service-fixture" || r.Header.Get("xi-api-key") != "" {
			t.Error("incorrect API endpoint or credential scope")
		}
		var body map[string]any
		if json.NewDecoder(r.Body).Decode(&body) != nil {
			t.Error("invalid JSON")
		}
		if body["input"] != "Bonjour." || body["model"] != "custom-tts" || body["voice"] != "custom-voice" || body["response_format"] != "wav" {
			t.Error("incorrect synthesis contract")
		}
		w.Header().Set("Content-Type", "audio/wav")
		w.Write(wav)
	}))
	defer server.Close()
	c := New()
	c.Configure(voice.Settings{TTSProvider: "openai", TTSURL: server.URL + "/v1", ServiceTTSModel: "custom-tts", ServiceVoice: "custom-voice", ServiceAPIKey: "service-fixture", APIKey: "never-send-eleven-key"})
	count := 0
	if err := c.Stream(context.Background(), "Bonjour.", func(pcm []int16) error { count += len(pcm); return nil }); err != nil {
		t.Fatal(err)
	}
	if count != 800 {
		t.Fatalf("wrong telephony sample count %d", count)
	}
}
