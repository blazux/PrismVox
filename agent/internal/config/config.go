// Package config loads agent configuration from the PostgreSQL config table.
package config

import (
	"context"
	"log"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

var defaults = map[string]string{
	"system_prompt":          "Tu es la standardiste virtuelle de l'entreprise. Réponds en français, avec des phrases courtes : tes réponses sont lues à voix haute.",
	"outbound_system_prompt": "Tu passes un appel téléphonique sortant pour le compte de l'entreprise. Phrases courtes, lues à voix haute, toujours en français. Accomplis la mission décrite dans la section « Mission », note les réponses avec take_message, puis termine l'appel avec end_call. Tu dois APPELER les outils, jamais seulement décrire l'action.",
	"greeting":               "Bonjour, vous êtes bien au standard. Que puis-je faire pour vous ?",
	// Greeting for a caller Prism recognises by their number. Spoken instead of the
	// one above, before a single token is generated — a colleague should not be told
	// they have reached the switchboard. One optional %s = their name; without it
	// the line is spoken verbatim.
	"greeting_known": "Bonjour %s, c'est l'assistant Prism. Je suis à l'écoute.",
	"rag_enabled":    "true",
	"mcp_enabled":    "false",

	// Blind-transfer handoff method: "bridge" (Asterisk dials the destination and
	// stays in the media path) or "refer" (Asterisk sends a SIP REFER to the
	// caller's softswitch and drops out, Centrex/phone-style). Lives with the SIP
	// config (sip_* keys, UI → Configuration → SIP).
	"sip_transfer_method": "bridge",

	// Spoken phrases used by the call-control code (overridable via the config table).
	"phrase_farewell":       "Au revoir, bonne journée !",
	"phrase_hold":           "Un instant, je vérifie sa disponibilité.",
	"phrase_transfer_now":   "Très bien, je lance le transfert.",
	"phrase_connecting":     "%s a accepté l'appel, je lance la mise en relation.",
	"phrase_ask_name":       "Quel nom dois-je annoncer ?",
	"phrase_ask_reason":     "Quel est le motif de l'appel ?",
	"phrase_still_there":    "Êtes-vous toujours en ligne ?",
	"phrase_not_understood": "Pardon, je n'ai pas bien entendu. Est-ce possible de répéter ?",
	// Short filler played to bridge the silence when a TTS take has to be regenerated.
	"phrase_filler":          "Un instant.",
	"phrase_unknown_contact": "Je suis désolée, je ne peux pas joindre %s. Puis-je prendre un message ?",
	// Attended-transfer announcement to the recipient. Two %s: caller name, then reason.
	"phrase_announce": "Bonjour, j'ai %s en ligne pour vous, à propos de : %s. Souhaitez-vous prendre l'appel ?",
}

// Config holds runtime agent configuration.
type Config struct {
	SystemPrompt string
	Greeting     string
	RAGEnabled   bool
	MCPEnabled   bool
	// Raw holds all keys (including custom ones) for extensibility.
	Raw map[string]string
}

// Load reads the config table and returns a populated Config.
// If pool is nil, defaults are used.
func Load(ctx context.Context, pool *pgxpool.Pool) (*Config, error) {
	raw := make(map[string]string, len(defaults))
	for k, v := range defaults {
		raw[k] = v
	}

	if pool != nil {
		rows, err := pool.Query(ctx, "SELECT key, value FROM config")
		if err != nil {
			log.Printf("config: query error: %v", err)
		} else {
			defer rows.Close()
			for rows.Next() {
				var k, v string
				if err := rows.Scan(&k, &v); err == nil {
					raw[k] = v
				}
			}
		}
	}

	return &Config{
		SystemPrompt: raw["system_prompt"],
		Greeting:     raw["greeting"],
		RAGEnabled:   raw["rag_enabled"] == "true",
		MCPEnabled:   raw["mcp_enabled"] == "true",
		Raw:          raw,
	}, nil
}

// FromMap builds a Config from raw key/values, without a database. Intended for
// tests and for call paths that already hold the config table's contents.
func FromMap(raw map[string]string) *Config {
	merged := make(map[string]string, len(defaults)+len(raw))
	for k, v := range defaults {
		merged[k] = v
	}
	for k, v := range raw {
		merged[k] = v
	}
	return &Config{
		SystemPrompt: merged["system_prompt"],
		Greeting:     merged["greeting"],
		RAGEnabled:   merged["rag_enabled"] == "true",
		MCPEnabled:   merged["mcp_enabled"] == "true",
		Raw:          merged,
	}
}

// Get returns the configured value for key, falling back to the compiled-in
// default when the key is absent or blank.
func (c *Config) Get(key string) string {
	if v, ok := c.Raw[key]; ok && strings.TrimSpace(v) != "" {
		return v
	}
	return defaults[key]
}
