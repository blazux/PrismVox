package call

import "testing"

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
