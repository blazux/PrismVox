// Package contacts loads the phone directory from the database and formats it
// for injection into the LLM system prompt.
package contacts

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Transfer types. Blind puts the caller straight through; attended announces
// them to the recipient first, who may decline.
const (
	TransferBlind    = "blind"
	TransferAttended = "attended"
)

// Contact is an enabled directory entry.
type Contact struct {
	Name         string
	PhoneNumber  string
	TransferType string // "blind" | "attended"
}

// Resolve finds the unique directory entry matching query, which may be an
// extension or a contact name (case-, accent- and hyphen-insensitive). Exact
// name/number first, then a whole word of the name ("Vincent" → "Vincent
// Dupont"). Two candidates at the same level = nil: with "Jean Dupont" and
// "Jean Martin" in the directory, "Jean" must make the agent ask, never guess.
// Nil also when nothing matches — the agent must never transfer to a
// destination that is not in the directory.
func Resolve(ctx context.Context, pool *pgxpool.Pool, query string) *Contact {
	if pool == nil {
		return nil
	}
	all, err := loadEnabled(ctx, pool)
	if err != nil || len(all) == 0 {
		return nil
	}
	return ResolveEntries(all, query)
}

// ResolveEntries is Resolve on an in-memory directory.
func ResolveEntries(all []Contact, query string) *Contact {
	q := strings.TrimSpace(query)
	qn := normalize(q)
	if qn == "" {
		return nil
	}
	for _, exact := range []bool{true, false} {
		var match *Contact
		for i := range all {
			n := normalize(all[i].Name)
			ok := n == qn || all[i].PhoneNumber == q
			if !exact {
				ok = strings.Contains(" "+n+" ", " "+qn+" ")
			}
			if ok {
				if match != nil {
					return nil // ambiguous
				}
				match = &all[i]
			}
		}
		if match != nil {
			return match
		}
	}
	return nil
}

// normalize lowercases, strips French accents and turns hyphens into spaces
// so "Jean-Marc" and "jean marc" compare equal.
func normalize(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	r := strings.NewReplacer(
		"é", "e", "è", "e", "ê", "e", "ë", "e",
		"à", "a", "â", "a", "ä", "a",
		"î", "i", "ï", "i",
		"ô", "o", "ö", "o",
		"ù", "u", "û", "u", "ü", "u",
		"ç", "c", "œ", "oe", "-", " ",
	)
	return strings.Join(strings.Fields(r.Replace(s)), " ")
}

func loadEnabled(ctx context.Context, pool *pgxpool.Pool) ([]Contact, error) {
	rows, err := pool.Query(ctx,
		"SELECT name, phone_number, COALESCE(transfer_type,'blind') FROM contacts WHERE enabled = TRUE",
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Contact
	for rows.Next() {
		var c Contact
		if err := rows.Scan(&c.Name, &c.PhoneNumber, &c.TransferType); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// GetPrompt returns a formatted directory block for the LLM system prompt.
// Returns an empty string if there are no enabled contacts.
func GetPrompt(ctx context.Context, pool *pgxpool.Pool) (string, error) {
	rows, err := pool.Query(ctx,
		"SELECT name, phone_number, notes, transfer_type FROM contacts WHERE enabled = TRUE ORDER BY name",
	)
	if err != nil {
		return "", fmt.Errorf("contacts: query error: %w", err)
	}
	defer rows.Close()

	type contact struct {
		name         string
		phoneNumber  string
		notes        string
		transferType string
	}
	var contacts []contact
	for rows.Next() {
		var c contact
		var notes *string
		var ttype *string
		if err := rows.Scan(&c.name, &c.phoneNumber, &notes, &ttype); err != nil {
			continue
		}
		if notes != nil {
			c.notes = *notes
		}
		if ttype != nil {
			c.transferType = *ttype
		} else {
			c.transferType = "blind"
		}
		contacts = append(contacts, c)
	}
	if len(contacts) == 0 {
		return "", nil
	}

	entries := make([]Contact, 0, len(contacts))
	notes := make([]string, 0, len(contacts))
	for _, c := range contacts {
		entries = append(entries, Contact{Name: c.name, PhoneNumber: c.phoneNumber, TransferType: c.transferType})
		notes = append(notes, c.notes)
	}
	return PromptFor(entries, notes), nil
}

// PromptFor renders a directory block for the system prompt from an in-memory
// list. notes is optional and index-aligned; pass nil when there are none.
//
// Split out of GetPrompt because the transfer directory no longer always comes
// from Vox's own table: docked, it comes from Prism, whose user profiles are the
// only place someone becomes transferable.
func PromptFor(contacts []Contact, notes []string) string {
	if len(contacts) == 0 {
		return ""
	}
	lines := []string{
		"## Annuaire",
		"Ne déclenche un transfert QUE si l'appelant demande explicitement à être mis en relation avec une personne (« passe-moi X », « je voudrais parler à X »). Le contact à joindre est la personne que l'appelant veut JOINDRE — jamais l'appelant lui-même. Si l'appelant se présente ou donne son propre nom, ce n'est PAS une demande de transfert : poursuis simplement la conversation.",
		"Pour transférer un appel (et seulement dans ce cas), utilise le tool transfer_call avec le NOM du contact tel qu'il apparaît ci-dessous (le système retrouve l'extension lui-même).",
		"Le type de transfert est indiqué entre crochets :",
		"- [blind] : appelle immédiatement transfer_call avec le nom du contact, sans demander le nom de l'appelant ni l'objet de l'appel. N'annonce pas le transfert avant : c'est l'appel de l'outil qui transfère, pas une phrase.",
		"- [attended] : demande d'abord le nom de l'appelant ET l'objet de l'appel en une seule question, puis ATTENDS sa réponse. N'appelle transfer_call qu'au tour SUIVANT, une fois que tu as vraiment ces deux informations — ne les invente jamais (pas de « Inconnu » ni d'objet supposé) et n'appelle pas l'outil dans le même tour que ta question.",
		"Si la personne demandée n'apparaît pas dans cet annuaire, dis simplement que tu ne peux pas la joindre et propose de prendre un message.",
		"Ne mentionne JAMAIS l'annuaire ni tes recherches à l'appelant : consulte-le silencieusement.",
	}
	for i, c := range contacts {
		kind := c.TransferType
		if kind == "" {
			kind = "blind"
		}
		entry := fmt.Sprintf("- %s : %s [%s]", c.Name, c.PhoneNumber, kind)
		if i < len(notes) && notes[i] != "" {
			entry += fmt.Sprintf("  (%s)", notes[i])
		}
		lines = append(lines, entry)
	}
	return strings.Join(lines, "\n")
}

// NormalizeTransferType clamps a transfer type to a value the call code knows.
// Anything unreadable — an empty field, an old row, a typo in an API payload —
// becomes blind. Erring the other way would open an announcement dialogue with a
// caller who is waiting on a line, on nothing more than a bad string.
func NormalizeTransferType(kind string) string {
	if strings.TrimSpace(strings.ToLower(kind)) == TransferAttended {
		return TransferAttended
	}
	return TransferBlind
}
