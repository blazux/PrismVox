package call

import (
	"phoneagent/internal/db"
	"phoneagent/internal/llm"
	"strings"
	"testing"
)

// The greeting is the first thing a caller hears, and it is rendered without a
// model in the loop — so a badly written template must degrade to something
// speakable, never to a format-verb artefact read out loud.
func TestKnownGreeting(t *testing.T) {
	for _, tc := range []struct{ name, template, caller, want string }{
		{
			"name substituted",
			"Bonjour %s, c'est votre assistant.", "Vincent",
			"Bonjour Vincent, c'est votre assistant.",
		},
		{
			"template without a placeholder is spoken as-is",
			"Bonjour, c'est votre assistant.", "Vincent",
			"Bonjour, c'est votre assistant.",
		},
		{
			"a percent that is not a verb survives",
			"Bonjour %s, 100% dispo.", "Vincent",
			"Bonjour Vincent, 100% dispo.",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := knownGreeting(tc.template, tc.caller); got != tc.want {
				t.Fatalf("knownGreeting() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestOpeningBecomesAssistantHistoryOnEveryCallPath(t *testing.T) {
	for _, mode := range []string{"standalone", "unknown-linked", "recognized", "outbound"} {
		t.Run(mode, func(t *testing.T) {
			client := &llm.Client{}
			if mode != "standalone" {
				client.PrismURL = "http://prism.invalid"
			}
			client.CallerKnown = mode == "recognized"
			h := &Handler{llmClient: client}
			if mode == "outbound" {
				h.task = &db.CallTask{}
			}
			opening := "Bonjour Alice, comment ça va ?"
			h.rememberOpening(opening)
			if len(h.history) != 1 || h.history[0].Role != "assistant" || h.history[0].Content != opening {
				t.Fatal("opening missing from assistant history")
			}
			if len(h.turns) != 1 || h.turns[0]["text"] != opening || h.turns[0]["role"] != "agent" {
				t.Fatal("opening missing or duplicated in call archive")
			}
			if mode == "recognized" {
				if len(client.GatewayContext) != 1 || !strings.Contains(client.GatewayContext[0], opening) {
					t.Fatal("Prism will not receive the spoken opening")
				}
			} else if len(client.GatewayContext) != 0 {
				t.Fatal("local call received unnecessary gateway context")
			}
		})
	}
	h := &Handler{}
	h.rememberOpening("   ")
	if len(h.history) > 0 || len(h.turns) > 0 {
		t.Fatal("empty opening entered history")
	}
}
