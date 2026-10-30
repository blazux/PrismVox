package call

import (
	"reflect"
	"testing"

	"phoneagent/internal/config"
)

// The echo classifier is what stands between a working switchboard and an agent
// that answers its own voice: the Digicel trunk returns our playback on the
// caller stream, clean enough for Whisper to transcribe it verbatim. Cases below
// are real transcripts from the field tests of 2026-07-08.
func TestTranscriptEchoesText(t *testing.T) {
	tests := []struct {
		name       string
		transcript string
		spoken     string
		want       bool
	}{
		{
			name:       "verbatim echo of a fragment",
			transcript: "Et bien, merci pour votre appel.",
			spoken:     "Bonjour ! Ça va bien, merci pour votre appel. Comment puis-je vous aider aujourd'hui ?",
			want:       true,
		},
		{
			name:       "echo of the closing sentence",
			transcript: "si vous avez d'autres questions",
			spoken:     "Parfait ! Prenez soin de vous. Si vous avez d'autres questions un jour, je serai là. Au revoir !",
			want:       true,
		},
		{
			name:       "garbled echo (one word misheard)",
			transcript: "Je suis un peu plus en pédagogie",
			spoken:     "Je comprends, je suis un peu plus lent que d'habitude. Mais je suis là pour vous écouter.",
			want:       true,
		},
		{
			name:       "caller reusing a few of our words is not echo",
			transcript: "oui transférez-moi à Vincent",
			spoken:     "Voulez-vous que je vous transfère à Vincent ?",
			want:       false,
		},
		{
			name:       "genuine caller answer",
			transcript: "non merci c'est tout",
			spoken:     "Souhaitez-vous autre chose ?",
			want:       false,
		},
		{
			name:       "short utterances never classified as echo",
			transcript: "oui",
			spoken:     "Voulez-vous que je prenne un message ? Oui ou non ?",
			want:       false,
		},
		{
			name:       "nothing spoken yet",
			transcript: "bonjour je voudrais parler à quelqu'un",
			spoken:     "",
			want:       false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := transcriptEchoesText(tt.transcript, tt.spoken); got != tt.want {
				t.Errorf("transcriptEchoesText(%q, ...) = %v, want %v", tt.transcript, got, tt.want)
			}
		})
	}
}

func TestIsSTTHallucination(t *testing.T) {
	// Discard video subtitles; ordinary acknowledgements remain valid speech.
	for _, s := range []string{"  Merci d'avoir regardé  ", "sous-titrage société radio-canada"} {
		if !isSTTHallucination(s) {
			t.Errorf("isSTTHallucination(%q) = false, want true", s)
		}
	}
	for _, s := range []string{"", "merci pour votre appel", "je voudrais parler à Vincent", "bye bye Vincent", "Thank you", "thank you.", "THANKS", "Bye!", "À bientôt"} {
		if isSTTHallucination(s) {
			t.Errorf("isSTTHallucination(%q) = true, want false", s)
		}
	}
}

func TestSplitSentences(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []string
	}{
		{
			name: "keeps delimiters, trims spaces",
			in:   "Bonjour ! Ça va bien.  Et vous ?",
			want: []string{"Bonjour !", "Ça va bien.", "Et vous ?"},
		},
		{"no delimiter yields one sentence", "un instant", []string{"un instant"}},
		{"empty input yields nothing", "   ", nil},
		{"collapses repeated punctuation", "Vraiment ?! Oui.", []string{"Vraiment ?!", "Oui."}},
		{"semicolon is a boundary", "Un ; deux", []string{"Un ;", "deux"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := SplitSentences(tt.in); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("SplitSentences(%q) = %#v, want %#v", tt.in, got, tt.want)
			}
		})
	}
}

// Chain-of-thought must never reach the TTS: it would be read aloud to the caller.
func TestStripGemmaThinking(t *testing.T) {
	tests := []struct{ in, want string }{
		{"<think>Le client demande…</think>Bonjour !", "Bonjour !"},
		{"<think>multi\nligne\n</think>  Réponse.  ", "Réponse."},
		{"Pas de balise ici", "Pas de balise ici"},
		{"<think>tout</think>", ""},
	}
	for _, tt := range tests {
		if got := StripGemmaThinking(tt.in); got != tt.want {
			t.Errorf("StripGemmaThinking(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestBuildCallerID(t *testing.T) {
	// An empty display name must yield a bare user, not `"" <user>`: PJSIP would
	// otherwise stamp its "Anonymous" default on outbound calls.
	tests := []struct {
		name string
		cfg  map[string]string
		want string
	}{
		{"name and user", map[string]string{"sip_username": "596019033", "sip_callerid_name": "IRIS"}, `"IRIS" <596019033>`},
		{"user only", map[string]string{"sip_username": "596019033"}, "596019033"},
		{"blank name is ignored", map[string]string{"sip_username": "596019033", "sip_callerid_name": "   "}, "596019033"},
		{"no user disables callerid", map[string]string{"sip_callerid_name": "IRIS"}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := BuildCallerID(config.FromMap(tt.cfg)); got != tt.want {
				t.Errorf("BuildCallerID(%v) = %q, want %q", tt.cfg, got, tt.want)
			}
		})
	}
}
