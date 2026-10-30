// Package config loads agent configuration from the PostgreSQL config table.
package config

import (
	"context"
	"log"
	"phoneagent/internal/locale"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

var defaults = map[string]string{
	"phrase_outbound_opening": "Hello, I'm calling on behalf of the company.",
	"system_prompt":           "You are the company's virtual receptionist. Keep spoken replies short, with no markdown or lists. Adapt to the caller's language. Use search_knowledge_base before answering factual questions about the company and rely only on its results. Use transfer_call only when the caller asks to reach a listed contact. Use take_message to record a confirmed message. Execute tools for actions rather than merely announcing them. Never invent facts, contacts or successful outcomes. Finish with end_call only when the caller clearly finishes.",
	"outbound_system_prompt":  "You are making a telephone call on behalf of the company. Keep spoken replies short and adapt to the other person's language. Complete the supplied mission, record confirmed information with take_message and finish with end_call when appropriate. Execute the tools rather than merely describing actions.",
	"greeting":                "Hello, you've reached reception. How can I help you?",
	// Greeting for a caller Prism recognises by their number. Spoken instead of the
	// one above, before a single token is generated — a colleague should not be told
	// they have reached the switchboard. One optional %s = their name; without it
	// the line is spoken verbatim.
	"greeting_known": "Hello %s, this is your Prism assistant. I'm listening.",
	"rag_enabled":    "true",
	"mcp_enabled":    "false",

	// Blind-transfer handoff method: "bridge" (Asterisk dials the destination and
	// stays in the media path) or "refer" (Asterisk sends a SIP REFER to the
	// caller's softswitch and drops out, Centrex/phone-style). Lives with the SIP
	// config (sip_* keys, UI → Configuration → SIP).
	"sip_transfer_method": "bridge",

	// Spoken phrases used by the call-control code (overridable via the config table).
	"phrase_farewell":       "Goodbye, have a good day!",
	"phrase_hold":           "One moment, I'll check their availability.",
	"phrase_transfer_now":   "All right, I'll transfer you now.",
	"phrase_connecting":     "%s has accepted the call. I'll connect you now.",
	"phrase_ask_name":       "What name should I announce?",
	"phrase_ask_reason":     "What is the call about?",
	"phrase_still_there":    "Are you still on the line?",
	"phrase_not_understood": "Sorry, I didn't catch that. Could you repeat it?",
	// Short filler played to bridge the silence when a TTS take has to be regenerated.
	"phrase_filler":          "One moment.",
	"phrase_unknown_contact": "I'm sorry, I can't reach %s. May I take a message?",
	// Attended-transfer announcement to the recipient. Two %s: caller name, then reason.
	"phrase_announce": "Hello, I have %s on the line about: %s. Would you like to take the call?",
}

// Config holds runtime agent configuration.
type Config struct {
	SystemPrompt string
	Greeting     string
	RAGEnabled   bool
	MCPEnabled   bool
	// Raw holds all keys (including custom ones) for extensibility.
	Raw        map[string]string
	Configured map[string]bool
}

// Load reads the config table and returns a populated Config.
// If pool is nil, defaults are used.
func Load(ctx context.Context, pool *pgxpool.Pool) (*Config, error) {
	raw := make(map[string]string, len(defaults))
	configured := map[string]bool{}
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
					configured[k] = true
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
		Configured:   configured,
	}, nil
}

// FromMap builds a Config from raw key/values, without a database. Intended for
// tests and for call paths that already hold the config table's contents.
func FromMap(raw map[string]string) *Config {
	merged := make(map[string]string, len(defaults)+len(raw))
	configured := map[string]bool{}
	for k, v := range defaults {
		merged[k] = v
	}
	for k, v := range raw {
		merged[k] = v
		configured[k] = true
	}
	return &Config{
		SystemPrompt: merged["system_prompt"],
		Greeting:     merged["greeting"],
		RAGEnabled:   merged["rag_enabled"] == "true",
		MCPEnabled:   merged["mcp_enabled"] == "true",
		Raw:          merged,
		Configured:   configured,
	}
}

// Get returns the configured value for key, falling back to the compiled-in
// default when the key is absent or blank.
func (c *Config) Get(key string) string {
	if v, ok := c.Raw[key]; ok && strings.TrimSpace(v) != "" {
		return v
	}
	if value := defaults[key]; value != "" {
		return value
	}
	return locale.Default("en", key)
}
