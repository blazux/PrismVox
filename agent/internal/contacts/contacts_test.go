package contacts

import (
	"strings"
	"testing"
)

func TestDirectoryNeverPicksAnAmbiguousContact(t *testing.T) {
	entries := []Contact{{Name: "Jean Dupont", PhoneNumber: "101"}, {Name: "Jean Martin", PhoneNumber: "102"}, {Name: "Gérard Blanc", PhoneNumber: "103"}}
	for _, query := range []string{"Jean", "", "an", "Unknown"} {
		if c := ResolveEntries(entries, query); c != nil {
			t.Fatalf("%q unexpectedly resolved to %s", query, c.Name)
		}
	}
	for query, phone := range map[string]string{"Jean Martin": "102", "102": "102", "gerard": "103"} {
		if c := ResolveEntries(entries, query); c == nil || c.PhoneNumber != phone {
			t.Fatalf("%q: got %+v", query, c)
		}
	}
	entries = append(entries, Contact{Name: "Jean Martin", PhoneNumber: "104"})
	if c := ResolveEntries(entries, "Jean Martin"); c != nil {
		t.Fatal("duplicate exact name must be ambiguous")
	}
}

func TestHyphenatedNamesMatchEitherSpelling(t *testing.T) {
	entries := []Contact{{Name: "Jean-Marc Dupont", PhoneNumber: "201"}, {Name: "Marie Curie", PhoneNumber: "202"}}
	for _, query := range []string{"Marc", "jean marc", "Jean-Marc", "jean-marc dupont"} {
		if c := ResolveEntries(entries, query); c == nil || c.PhoneNumber != "201" {
			t.Fatalf("%q: got %+v", query, c)
		}
	}
}

// The transfer type decides whether a stranger is put straight onto someone's
// phone or announced first. Every unreadable value must land on blind — the safe
// side is the one that does not start an announcement dialogue with a caller
// waiting on the line because of a typo.
func TestNormalizeTransferType(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"attended", TransferAttended},
		{"ATTENDED", TransferAttended},
		{"  attended  ", TransferAttended},
		{"blind", TransferBlind},
		{"", TransferBlind},
		{"supervised", TransferBlind},
		{"attendu", TransferBlind},
	} {
		if got := NormalizeTransferType(tc.in); got != tc.want {
			t.Errorf("NormalizeTransferType(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// A directory rendered for the prompt must label every entry, including rows that
// predate the column: an unlabelled name is one the model has to guess about.
func TestPromptForLabelsEveryEntry(t *testing.T) {
	got := PromptFor([]Contact{
		{Name: "Gérard Blanc", PhoneNumber: "103", TransferType: TransferAttended},
		{Name: "Jean Dupont", PhoneNumber: "101"},
	}, nil)
	for _, want := range []string{"Gérard Blanc : 103 [attended]", "Jean Dupont : 101 [blind]"} {
		if !strings.Contains(got, want) {
			t.Errorf("prompt missing %q\n%s", want, got)
		}
	}
	if PromptFor(nil, nil) != "" {
		t.Error("an empty directory must render nothing at all")
	}
}
