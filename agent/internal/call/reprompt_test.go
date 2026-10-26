package call

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"phoneagent/internal/config"
	"phoneagent/internal/llm"
)

// The generated re-prompt must never leave the line silent. With no history there
// is nothing to reference, so the fixed phrase is used without even calling the
// LLM — this also guarantees the very first silence (before any exchange) never
// waits on the network.
func TestGenerateRepromptFallsBackWithoutHistory(t *testing.T) {
	h := &Handler{channelID: "test"}
	cfg := config.FromMap(map[string]string{"phrase_still_there": "Êtes-vous toujours en ligne ?"})

	got := h.generateReprompt(context.Background(), cfg, 1)
	if want := "Êtes-vous toujours en ligne ?"; got != want {
		t.Errorf("generateReprompt() = %q, want the configured fallback %q", got, want)
	}
}

// A caller-configured phrase must win over the compiled-in default.
func TestGenerateRepromptUsesConfiguredPhrase(t *testing.T) {
	h := &Handler{channelID: "test"}
	cfg := config.FromMap(map[string]string{"phrase_still_there": "Allô ?"})
	if got := h.generateReprompt(context.Background(), cfg, 2); got != "Allô ?" {
		t.Errorf("generateReprompt() = %q, want %q", got, "Allô ?")
	}
}

// Exercise the actual HTTP boundary: a nudge must retain recent replies and
// never inherit the switchboard persona, execute tools, or route through Prism.
func TestGenerateRepromptRequestAndFallbacks(t *testing.T) {
	for _, tc := range []struct {
		name   string
		known  bool
		status int
		reply  string
		want   string
	}{
		{"unknown", false, 200, "Concernant les horaires, êtes-vous toujours là ?", "Concernant les horaires, êtes-vous toujours là ?"},
		{"known", true, 200, "À propos de ton rendez-vous, toujours là ?", "À propos de ton rendez-vous, toujours là ?"},
		{"empty", true, 200, "", "Allô ?"},
		{"failed", true, 503, "", "Allô ?"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/chat/completions" {
					t.Errorf("unexpected endpoint: %s", r.URL.Path)
				}
				var body struct {
					Messages []llm.Message `json:"messages"`
					Tools    []any         `json:"tools"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
					return
				}
				if len(body.Messages) != 4 || len(body.Tools) != 0 {
					t.Errorf("history or tool-free request lost: %+v", body)
					return
				}
				prompt, _ := body.Messages[0].Content.(string)
				if strings.Contains(prompt, "SWITCHBOARD_PERSONA") || !strings.Contains(prompt, repromptPrompt) {
					t.Errorf("wrong nudge persona: %q", prompt)
				}
				if tc.known != strings.Contains(prompt, "Reprends le tutoiement ou le vouvoiement") {
					t.Errorf("wrong register rule: %q", prompt)
				}
				if !tc.known && !strings.Contains(prompt, "vouvoie-le toujours, même s'il tutoie") {
					t.Errorf("unknown caller must be addressed formally: %q", prompt)
				}
				if body.Messages[2].Content != "Tu veux le rendez-vous de demain ?" || !strings.Contains(body.Messages[3].Content.(string), "deuxième relance") {
					t.Error("recent assistant reply or retry instruction lost")
				}
				w.WriteHeader(tc.status)
				json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": tc.reply}}}})
			}))
			defer server.Close()
			t.Setenv("OPENAI_BASE_URL", server.URL)
			h := &Handler{llmClient: llm.New(), history: []llm.Message{
				{Role: "user", Content: "Le rendez-vous."},
				{Role: "assistant", Content: "Tu veux le rendez-vous de demain ?"},
			}}
			h.llmClient.PrismURL = "http://prism.invalid"
			h.llmClient.CallerKnown = tc.known
			cfg := config.FromMap(map[string]string{"system_prompt": "SWITCHBOARD_PERSONA", "phrase_still_there": "Allô ?"})
			if got := h.generateReprompt(context.Background(), cfg, 2); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}
