// Package call provides call handling logic, tool definitions, and text utilities.
package call

import (
	"regexp"
	"strings"
	"unicode"
)

// TransferTool is the built-in transfer_call tool in OpenAI function format.
// The destination is resolved against the directory in Go — the LLM passes the
// contact NAME, never a free-form number, so hallucinated extensions are rejected.
var TransferTool = map[string]interface{}{
	"type": "function",
	"function": map[string]interface{}{
		"name": "transfer_call",
		"description": "Transfer the current call to a person from the directory — ONLY when the caller explicitly asks to be connected to that person. " +
			"Calling this tool IS the transfer: never just say you are transferring without calling it. " +
			"Pass the contact's name exactly as it appears in the directory (the system resolves the extension); the destination is the person the caller wants to reach, never the caller themselves. " +
			"For [blind] contacts: call this tool immediately. " +
			"For [attended] contacts: only call it once the caller has actually given their name AND the reason. " +
			"If you don't have both yet, ask for them in a SINGLE question and WAIT for the caller's reply — do NOT call this tool in the same turn as your question, and never invent caller_name or reason.",
		"parameters": map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"destination": map[string]interface{}{
					"type":        "string",
					"description": "Name of the directory contact to transfer to, e.g. Vincent",
				},
				"caller_name": map[string]interface{}{
					"type": "string",
					"description": "Caller's name, EXACTLY as they said it. Omit this field entirely " +
						"if they have not stated their name — the switchboard will ask them. Never guess, " +
						"never write a placeholder like \"Inconnu\" or \"M. Dupont\".",
				},
				"reason": map[string]interface{}{
					"type": "string",
					"description": "Why the caller wants to reach this person, in their own words. Omit " +
						"this field entirely if they have not said why — the switchboard will ask them. " +
						"\"Demande de transfert\" is not a reason.",
				},
			},
			"required": []string{"destination"},
		},
	},
}

// SearchKnowledgeTool lets the LLM query the knowledge base on demand (agentic RAG)
// instead of having the whole KB injected every turn. It is a read-only tool — safe
// to call eagerly — resolved in Go via the embeddings service + pgvector.
var SearchKnowledgeTool = map[string]interface{}{
	"type": "function",
	"function": map[string]interface{}{
		"name": "search_knowledge_base",
		"description": "Search the company knowledge base. Call this BEFORE answering any factual question about the company, its services, prices, procedures, hours, or any specific information — do not answer such questions from memory. " +
			"Pass a focused query: reformulate the caller's question into clear keywords (e.g. \"prix abonnement Pro\", \"horaires support\") rather than the raw sentence. " +
			"It returns the most relevant passages; answer ONLY from them and never invent. If nothing relevant comes back, tell the caller you don't have that information.",
		"parameters": map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"query": map[string]interface{}{
					"type":        "string",
					"description": "What to look up, as focused search keywords",
				},
			},
			"required": []string{"query"},
		},
	},
}

// EndCallTool lets the LLM terminate the call once the caller has no further needs.
var EndCallTool = map[string]interface{}{
	"type": "function",
	"function": map[string]interface{}{
		"name": "end_call",
		"description": "End the current call — ONLY when the caller has clearly signalled they are finished " +
			"(an explicit goodbye, \"c'est tout merci\", \"au revoir\"). " +
			"Do NOT end after answering a single question, on a brief acknowledgement (\"ok\", \"d'accord\", \"merci\"), " +
			"or when you are unsure — keep the conversation going and let the caller lead. " +
			"The system speaks a polite farewell before hanging up.",
		"parameters": map[string]interface{}{
			"type":       "object",
			"properties": map[string]interface{}{},
		},
	},
}

// AcceptCallTool / DeclineCallTool are only exposed during the attended-transfer
// announcement dialog with the transfer recipient. Tool calls are far more
// reliable than parsing decision tags out of spoken text.
var AcceptCallTool = map[string]interface{}{
	"type": "function",
	"function": map[string]interface{}{
		"name":        "accept_call",
		"description": "The recipient clearly agreed to take the waiting call. Call this immediately when they say yes.",
		"parameters": map[string]interface{}{
			"type":       "object",
			"properties": map[string]interface{}{},
		},
	},
}

var DeclineCallTool = map[string]interface{}{
	"type": "function",
	"function": map[string]interface{}{
		"name":        "decline_call",
		"description": "The recipient clearly refused to take the waiting call (busy, unavailable, not interested). Call this immediately when they say no.",
		"parameters": map[string]interface{}{
			"type":       "object",
			"properties": map[string]interface{}{},
		},
	},
}

// TakeMessageTool is the built-in take_message tool in OpenAI function format.
var TakeMessageTool = map[string]interface{}{
	"type": "function",
	"function": map[string]interface{}{
		"name": "take_message",
		"description": "Record a message from the caller to be delivered to a recipient. " +
			"Use it when the caller asks to leave a message, or when the person they want is unavailable. " +
			"Confirm the message back to the caller before calling this tool.",
		"parameters": map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"recipient": map[string]interface{}{
					"type":        "string",
					"description": "Name of the message recipient",
				},
				"content": map[string]interface{}{
					"type":        "string",
					"description": "Full content of the message",
				},
			},
			"required": []string{"recipient", "content"},
		},
	},
}

// thinkingRE strips Qwen3.x <think>...</think> chain-of-thought wrappers that may
// slip past enable_thinking:false / the reasoning parser, before they reach TTS.
var thinkingRE = regexp.MustCompile(`(?s)<think>.*?</think>`)

// StripGemmaThinking removes model chain-of-thought wrappers before TTS.
// (Name is historical — the project moved Gemma-4 → Qwen3.5; see CLAUDE.md.)
func StripGemmaThinking(text string) string {
	return strings.TrimSpace(thinkingRE.ReplaceAllString(text, ""))
}

// sttHallucinations are stock phrases Whisper emits on noise/near-silence (its
// training data ends videos with them). Ordinary acknowledgements in any
// language are accepted; acoustic and transcript echo checks handle playback.
var sttHallucinations = map[string]bool{
	"sous-titres réalisés par la communauté d'amara.org": true,
	"sous-titrage société radio-canada":                  true,
	"merci d'avoir regardé cette vidéo":                  true, "merci d'avoir regardé": true,
	"c'est la fin de la vidéo": true,
}

// isSTTHallucination reports whether a transcript is one of Whisper's known
// noise hallucinations (compared case/punctuation-insensitively).
func isSTTHallucination(text string) bool {
	t := strings.ToLower(strings.TrimSpace(text))
	t = strings.Trim(t, ".!… ")
	return sttHallucinations[t]
}

// normalizeWords lowercases a spoken/transcribed sentence and splits it into
// words, dropping punctuation, for fuzzy transcript comparison.
func normalizeWords(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r)
	})
}

// transcriptEchoesText reports whether a barge-in transcript is mostly a
// fragment of what the agent itself was saying — i.e. line echo clean enough
// for Whisper to transcribe (observed on the Digicel trunk: the agent's own
// sentences came back verbatim and it started answering itself). At least 70%
// of the transcript's words (minimum 3) must appear in the spoken text — high
// enough that a caller genuinely reusing a few of the agent's words stays
// below it (~40% in practice), low enough to catch garbled echo.
func transcriptEchoesText(transcript, spoken string) bool {
	tw := normalizeWords(transcript)
	if len(tw) < 3 || spoken == "" {
		return false
	}
	sw := make(map[string]bool)
	for _, w := range normalizeWords(spoken) {
		sw[w] = true
	}
	hits := 0
	for _, w := range tw {
		if sw[w] {
			hits++
		}
	}
	return float64(hits) >= 0.7*float64(len(tw))
}

// placeholderValues are the strings models emit for an attended transfer when they
// feel obliged to fill caller_name/reason but the caller never gave them. Observed
// live: caller="Inconnu", caller="M. Dupont" (pure invention), reason="Demande de
// transfert" (a restatement of the request, not a reason).
var placeholderValues = map[string]bool{
	"inconnu": true, "inconnue": true, "unknown": true, "n/a": true, "na": true,
	"non communiqué": true, "non communique": true, "non précisé": true, "non precise": true,
	"non spécifié": true, "non specifie": true, "non renseigné": true, "non renseigne": true,
	"aucune": true, "aucun": true, "sans objet": true, "anonyme": true, "client": true,
	"appelant": true, "l'appelant": true, "caller": true, "m. dupont": true,
	"demande de transfert": true, "transfert": true, "demande de mise en relation": true,
	"mise en relation": true, "souhaite parler": true, "appel": true,
}

// isPlaceholder reports whether the model filled a field with a stand-in rather
// than something the caller actually said.
func isPlaceholder(v string) bool {
	t := strings.ToLower(strings.TrimSpace(v))
	t = strings.Trim(t, ".!?… ")
	return t == "" || placeholderValues[t]
}

// callerSaidName reports whether any distinctive word of name appears in what the
// caller actually uttered. An attended transfer announces the caller to a colleague,
// so the name must come from the caller, not from the model's imagination — the same
// rule as transfer destinations, which are resolved against the directory rather
// than trusted from the tool call.
func callerSaidName(name string, callerTurns []string) bool {
	said := make(map[string]bool)
	for _, t := range callerTurns {
		for _, w := range normalizeWords(t) {
			said[w] = true
		}
	}
	for _, w := range normalizeWords(name) {
		if len(w) >= 3 && said[w] { // skip "de", "le", initials
			return true
		}
	}
	return false
}

// sanitizeAttendedInfo strips the caller_name / reason the model supplied for an
// attended transfer whenever they are not grounded in what the caller said. An
// empty result means "ask the caller" — the switchboard does exactly that.
//
// This is the single decision point: the handler must not re-implement it, so a
// test on this function covers the behaviour that reaches the phone line.
func sanitizeAttendedInfo(callerName, reason string, callerTurns []string) (string, string) {
	if isPlaceholder(callerName) || !callerSaidName(callerName, callerTurns) {
		callerName = ""
	}
	if isPlaceholder(reason) {
		reason = ""
	}
	return callerName, reason
}

// sentenceSplitRE matches sentence-ending punctuation.
var sentenceSplitRE = regexp.MustCompile(`[.?!;]+`)

// SplitSentences splits text into sentences on ., ?, !, ; boundaries.
// It preserves the delimiter at the end of each sentence and trims whitespace.
func SplitSentences(text string) []string {
	if strings.TrimSpace(text) == "" {
		return nil
	}

	var sentences []string
	remaining := text

	for {
		loc := sentenceSplitRE.FindStringIndex(remaining)
		if loc == nil {
			if s := strings.TrimSpace(remaining); s != "" {
				sentences = append(sentences, s)
			}
			break
		}
		end := loc[1]
		s := strings.TrimSpace(remaining[:end])
		if s != "" {
			sentences = append(sentences, s)
		}
		remaining = remaining[end:]
	}

	return sentences
}
