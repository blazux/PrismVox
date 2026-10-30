package locale

import "testing"

func TestDetectionAndAmbiguousSpeech(t *testing.T) {
	for _, tc := range []struct{ text, code string }{
		{"I would like to arrange an appointment tomorrow morning, please.", "en"},
		{"Je voudrais prendre un rendez-vous demain matin, s'il vous plaît.", "fr"},
		{"Me gustaría reservar una cita para mañana por la mañana, por favor.", "es"},
		{"OK", ""}, {"Vincent", ""}, {"SIP", ""},
	} {
		if got := Detect(tc.text); got != tc.code {
			t.Errorf("%q: %s, want %s", tc.text, got, tc.code)
		}
	}
	if Normalize("fra") != "fr" || Normalize("en-US") != "en" {
		t.Fatal("language normalization")
	}
}

func TestCatalogueIntegrityAndPlaceholderValidation(t *testing.T) {
	for _, code := range []string{"en", "fr"} {
		if !ValidTranslation(Defaults("en"), Defaults(code)) {
			t.Fatal("invalid built-in catalogue", code)
		}
	}
	source := map[string]string{"phrase": "Hello %s, 100% available"}
	if ValidTranslation(source, map[string]string{"phrase": "Bonjour"}) {
		t.Fatal("missing placeholder accepted")
	}
	if !ValidTranslation(source, map[string]string{"phrase": "Bonjour %s, disponible à 100%"}) {
		t.Fatal("literal percent rejected")
	}
	if Signature(map[string]string{"b": "é", "a": "日本"}) != Signature(map[string]string{"a": "日本", "b": "é"}) {
		t.Fatal("unstable signature")
	}
}
