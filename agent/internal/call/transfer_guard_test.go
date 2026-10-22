package call

import "testing"

// Real values observed on live calls: the model filled caller_name/reason rather
// than leaving them out, so the "ask the caller" fallback never fired.
func TestIsPlaceholder(t *testing.T) {
	fabricated := []string{
		"", "Inconnu", "inconnue", "Unknown", "N/A", "non communiqué", "Non précisé",
		"Aucune", "anonyme", "Demande de transfert", "demande de transfert.", "Transfert",
		"appelant", "Client",
	}
	for _, v := range fabricated {
		if !isPlaceholder(v) {
			t.Errorf("isPlaceholder(%q) = false, want true", v)
		}
	}
	genuine := []string{"Gérard Martin", "Vincent", "une panne sur la ligne fibre", "il veut un devis"}
	for _, v := range genuine {
		if isPlaceholder(v) {
			t.Errorf("isPlaceholder(%q) = true, want false", v)
		}
	}
}

// The name must appear in what the caller uttered — the model cannot conjure it.
func TestCallerSaidName(t *testing.T) {
	turns := []string{
		"Bonjour, je m'appelle Gérard Martin",
		"je voudrais parler à Vincent s'il vous plaît",
	}
	if !callerSaidName("Gérard Martin", turns) {
		t.Error("a name the caller stated must be accepted")
	}
	if !callerSaidName("Gérard", turns) {
		t.Error("a first name the caller stated must be accepted")
	}
	if callerSaidName("M. Dupont", turns) {
		t.Error("a name the caller never said must be rejected (this is the live bug)")
	}
	if callerSaidName("Inconnu", turns) {
		t.Error("a placeholder must not pass the history check")
	}
	// Short tokens must not carry a match on their own.
	if callerSaidName("de la", []string{"je voudrais parler de la panne"}) {
		t.Error("words shorter than 3 letters must not validate a name")
	}
}

// The decision the phone line actually takes. Empty means "ask the caller".
func TestSanitizeAttendedInfo(t *testing.T) {
	turns := []string{"bonjour je m'appelle Gérard", "passez-moi Vincent pour une panne fibre"}

	// The live bug: both fields invented, both must be dropped so the agent asks.
	if n, r := sanitizeAttendedInfo("M. Dupont", "Demande de transfert", turns); n != "" || r != "" {
		t.Errorf("fabricated pair kept: name=%q reason=%q — the agent would announce a lie", n, r)
	}
	// A plausible name the caller never uttered — no blocklist can enumerate these,
	// only the history check catches it.
	if n, _ := sanitizeAttendedInfo("Jean Martin", "une panne fibre", turns); n != "" {
		t.Errorf("name %q was never said by the caller but survived", n)
	}
	// Genuine values survive untouched.
	if n, r := sanitizeAttendedInfo("Gérard", "une panne fibre", turns); n != "Gérard" || r != "une panne fibre" {
		t.Errorf("genuine pair altered: name=%q reason=%q", n, r)
	}
	// A real name with a placeholder reason: keep the name, ask the reason.
	if n, r := sanitizeAttendedInfo("Gérard", "Inconnu", turns); n != "Gérard" || r != "" {
		t.Errorf("expected name kept and reason dropped, got name=%q reason=%q", n, r)
	}
	// Nothing supplied at all: ask for both.
	if n, r := sanitizeAttendedInfo("", "", turns); n != "" || r != "" {
		t.Errorf("empty input must stay empty, got name=%q reason=%q", n, r)
	}
}
