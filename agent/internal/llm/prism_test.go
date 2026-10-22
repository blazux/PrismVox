package llm

import "testing"

// Regression: Prism is a dashboard agent — it writes emoji and markdown. Spoken
// verbatim, the TTS reads asterisks and pictographs aloud (heard live on the first
// docked call).
func TestSanitizeForSpeech(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Bonjour ! 👋 Comment puis-je aider ?", "Bonjour ! Comment puis-je aider ?"},
		{"C'est **important** et `urgent`.", "C'est important et urgent."},
		{"- Premier point\n- Second point", "Premier point Second point"},
		{"## Titre\nDu texte.", "Titre Du texte."},
		{"Voir [le site](https://exemple.fr) svp.", "Voir le site svp."},
		{"Terminé ✅ tout va bien 🎉", "Terminé tout va bien"},
		{"Rien à nettoyer.", "Rien à nettoyer."},
	}
	for _, c := range cases {
		if got := sanitizeForSpeech(c.in); got != c.want {
			t.Errorf("sanitizeForSpeech(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// partialTagTail keeps a "<think>" tag from being spoken when it is split across
// two stream chunks (e.g. "…texte <thi" + "nk>raisonnement</think>").
func TestPartialTagTail(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"du texte <thi", 4},
		{"du texte <", 1},
		{"du texte", 0},
		{"finit par <think", 6},
		{"", 0},
	}
	for _, c := range cases {
		if got := partialTagTail(c.in); got != c.want {
			t.Errorf("partialTagTail(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}
