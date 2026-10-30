package call

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"phoneagent/internal/config"
	"phoneagent/internal/llm"
	"phoneagent/internal/locale"
	"phoneagent/internal/stt"
	"testing"
	"time"
)

func TestDefaultEnglishAndLanguageSwitchIsCallLocal(t *testing.T) {
	cfg := config.FromMap(nil)
	if cfg.Greeting != locale.Default("en", "greeting") {
		t.Fatal("new configuration is not English")
	}
	h := &Handler{sttClient: stt.New()}
	other := &Handler{}
	h.initLanguage(context.Background(), cfg, "en", true)
	other.initLanguage(context.Background(), cfg, "en", true)
	if h.phrase(cfg, "phrase_still_there") != "Are you still on the line?" {
		t.Fatal("English fallback missing")
	}
	h.observeSpeech(context.Background(), "Je voudrais prendre un rendez-vous demain matin, s'il vous plaît.")
	if h.currentLanguage() != "fr" || h.phrase(cfg, "phrase_still_there") != "Êtes-vous toujours en ligne ?" {
		t.Fatal("French switch or offline fallback missing")
	}
	h.observeSpeech(context.Background(), "OK")
	if h.currentLanguage() != "fr" || other.currentLanguage() != "en" {
		t.Fatal("ambiguous word or another call changed language")
	}
	h.observeSpeech(context.Background(), "I would like to arrange an appointment tomorrow morning, please.")
	if h.currentLanguage() != "en" {
		t.Fatal("switch back to English failed")
	}
	h.initLanguage(context.Background(), cfg, "en", false)
	h.observeSpeech(context.Background(), "Je voudrais prendre un rendez-vous demain matin, s'il vous plaît.")
	if h.currentLanguage() != "en" {
		t.Fatal("fixed language ignored")
	}
}

func TestLocalizedFillersNeverMixLanguages(t *testing.T) {
	h := &Handler{waitCues: []waitingClip{{Text: "Hmm…", Neutral: true}, {Text: "Alors…", Language: "fr"}, {Text: "One moment…", Language: "en"}}}
	cfg := config.FromMap(nil)
	h.initLanguage(context.Background(), cfg, "en", true)
	clips := h.localWaitingClips()
	if len(clips) != 2 || clips[1].Language != "en" {
		t.Fatal("foreign filler on English call")
	}
	h.setLanguage(context.Background(), "fr")
	clips = h.localWaitingClips()
	if len(clips) != 2 || clips[1].Language != "fr" {
		t.Fatal("English filler on French call")
	}
	h.setLanguage(context.Background(), "ja")
	clips = h.localWaitingClips()
	if len(clips) != 1 || !clips[0].Neutral {
		t.Fatal("missing pack must use a neutral filler")
	}
	if renderPhrase("Hello %s, 100% available", "Alice") != "Hello Alice, 100% available" || renderPhrase("", "Alice") != "" {
		t.Fatal("unsafe placeholder substitution")
	}
}

func TestUnconfiguredPhrasesFollowDefaultLanguageButCustomPhrasesSurvive(t *testing.T) {
	ctx := context.Background()
	cfg := config.FromMap(map[string]string{"greeting": "Bienvenue chez Acme"})
	h := &Handler{}
	h.initLanguage(ctx, cfg, "fr", true)
	if h.phrase(cfg, "greeting") != "Bienvenue chez Acme" {
		t.Fatal("custom greeting overwritten")
	}
	if got := knownGreeting(h.phrase(cfg, "greeting_known"), "Alice"); got != "Bonjour Alice, c'est l'assistant Prism. Je suis à l'écoute." {
		t.Fatalf("unconfigured recognized-caller greeting in wrong language: %s", got)
	}
}

func TestDelayedRecognitionCannotUndoTheSpokenLanguage(t *testing.T) {
	cfg := config.FromMap(nil)
	h := &Handler{}
	ctx := context.Background()
	h.initLanguage(ctx, cfg, "en", true)
	request := "Could we please speak in French for the rest of this conversation?"
	h.acceptCallerText(ctx, request)
	h.observeSpeech(ctx, "Oui, bien sûr. Je peux vous aider à organiser votre rendez-vous.")
	if h.currentLanguage() != "fr" {
		t.Fatal("assistant language not observed")
	}
	h.observeCallerHint(ctx, "en", request)
	if h.currentLanguage() != "fr" {
		t.Fatal("late recognition overrode the assistant's response")
	}
	h.acceptCallerText(ctx, "OK")
	if h.currentLanguage() != "fr" {
		t.Fatal("short acknowledgement changed language")
	}
}

func TestCustomizedTranslationsArePreparedOutsideTheConversation(t *testing.T) {
	cfg := config.FromMap(map[string]string{"phrase_still_there": "Are you still with us?"})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
			Tools []any `json:"tools"`
		}
		if json.NewDecoder(r.Body).Decode(&body) != nil || len(body.Messages) != 2 || len(body.Tools) != 0 {
			t.Error("unexpected translation request")
		}
		var source map[string]string
		if json.Unmarshal([]byte(body.Messages[1].Content), &source) != nil || source["phrase_still_there"] != "Are you still with us?" {
			t.Error("custom text lost")
		}
		phrases := locale.Defaults("fr")
		phrases["phrase_still_there"] = "Êtes-vous encore avec nous ?"
		data, _ := json.Marshal(phrases)
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"content": string(data)}}}})
	}))
	defer server.Close()
	t.Setenv("OPENAI_BASE_URL", server.URL)
	h := &Handler{llmClient: llm.New()}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	h.initLanguage(ctx, cfg, "en", true)
	h.setLanguage(ctx, "fr")
	for h.phrase(cfg, "phrase_still_there") != "Êtes-vous encore avec nous ?" {
		select {
		case <-ctx.Done():
			t.Fatal("custom translation unavailable")
		case <-time.After(time.Millisecond):
		}
	}
}
