package call

import (
	"context"
	"testing"

	"phoneagent/internal/config"
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
