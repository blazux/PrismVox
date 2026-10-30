package llm

// Prism docking clamp: when PRISM_URL is set, the caller turn is not
// answered by the local SGLang loop but by Prism's shared agent (its RAG, MCP
// tools, memory, personality). Vox becomes the voice organ; Prism becomes the
// brain. The connection is a single WebSocket to Prism's browser-agent surface
// (/ws), authenticated with the service token — the same protocol Prism serves
// to its own dashboard, so nothing on the Prism side is special-cased for us.
//
// Two things Prism's dashboard agent does that a phone line cannot tolerate, and
// that we absorb here:
//   - it streams <think>…</think> reasoning → filtered out, never spoken;
//   - it writes for a screen (emoji, markdown) → sanitised, or the TTS reads
//     "sparkles emoji" and asterisks out loud.
//
// Sentences are emitted as soon as they complete (not at end of turn), so TTS
// starts speaking while Prism is still writing.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	neturl "net/url"
	"phoneagent/internal/privacy"
	"regexp"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

// newSessionID returns a random id used as the Prism conversation session for
// one call. Prism persists history per session, so reusing this id across the
// call's turns keeps the conversation coherent server-side.
func newSessionID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "vox-call"
	}
	return "vox-" + hex.EncodeToString(b)
}

// streamPrism answers the caller turn via Prism's agent over WebSocket. It
// ignores the local systemPrompt/history/tools: Prism owns the brain. Complete
// sentences are streamed to sentCh as they form.
func (c *Client) streamPrism(ctx context.Context, userText string, sentCh chan<- string, toolsCh chan<- []ToolCall) error {
	header := http.Header{}
	if c.PrismToken != "" {
		header.Set("Authorization", "Bearer "+c.PrismToken)
	}
	// Declare the channel and who is calling. Prism trusts these only because we
	// hold the service token; on a voice channel it downgrades the connection to a
	// guest identity (restricted tools, switchboard persona) until it can identify
	// the caller from the number.
	q := neturl.Values{}
	q.Set("session", c.SessionID)
	q.Set("channel", "voice")
	if c.Caller != "" {
		q.Set("caller", c.Caller)
	}
	url, err := prismSocketURL(c.PrismURL, q)
	if err != nil {
		return err
	}

	dialer := *websocket.DefaultDialer
	dialer.HandshakeTimeout = 5 * time.Second
	requestStarted := time.Now()
	conn, _, err := dialer.DialContext(ctx, url, header)
	if err != nil {
		return fmt.Errorf("prism dial: %w", err)
	}
	defer conn.Close()
	connectedAt := time.Now()
	firstSpokenLogged := false

	// Fire the turn, declaring the voice channel: Prism then answers in spoken form
	// (short, no markup/emoji) and skips extended reasoning — the caller is waiting
	// in silence. Identity/personality are unchanged; only the form is.
	// Prism streams initial dashboard state first (container_status, file_tree,
	// tools_list, chat_history…) which we simply skip.
	gatewayContext := append([]string(nil), c.GatewayContext...)
	if c.LanguageHint != "" {
		gatewayContext = append(gatewayContext, c.LanguageHint)
	}
	if err := conn.WriteJSON(map[string]interface{}{
		"type":            "chat",
		"channel":         "voice",
		"content":         userText,
		"gateway_context": gatewayContext,
	}); err != nil {
		return fmt.Errorf("prism send: %w", err)
	}

	// Close the socket if the call context is cancelled (hangup / barge-in) so the
	// blocking ReadMessage below unblocks.
	watchDone := make(chan struct{})
	defer close(watchDone)
	go func() {
		select {
		case <-ctx.Done():
			conn.Close()
		case <-watchDone:
		}
	}()

	var (
		pending strings.Builder // raw stream text not yet classified (think vs spoken)
		spoken  strings.Builder // spoken text awaiting a sentence boundary
		inThink bool
		nSent   int
		relayed []ToolCall // telephony tools Prism asked Vox to perform (transfer, hang up…)
	)

	// flushSentences emits every complete sentence currently in `spoken`.
	flushSentences := func() error {
		for {
			s := spoken.String()
			idx := sentenceBoundary(s)
			if idx < 0 {
				return nil
			}
			sent := strings.TrimSpace(s[:idx+1])
			rest := s[idx+1:]
			spoken.Reset()
			spoken.WriteString(rest)
			if sent = sanitizeForSpeech(sent); sent == "" {
				continue
			}
			nSent++
			select {
			case sentCh <- sent:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}

	// classify moves text from `pending` into `spoken`, dropping <think> blocks.
	classify := func() {
		for {
			p := pending.String()
			if inThink {
				end := strings.Index(p, "</think>")
				if end < 0 {
					// The closing marker can straddle stream events too.
					keep := partialMarkerTail(p, "</think>")
					pending.Reset()
					pending.WriteString(p[len(p)-keep:])
					return
				}
				rest := p[end+len("</think>"):]
				pending.Reset()
				pending.WriteString(rest)
				inThink = false
				continue
			}
			start := strings.Index(p, "<think>")
			if start < 0 {
				// No think marker: everything so far is spoken. Keep a small tail back
				// in case a "<think>" tag is split across two chunks.
				if keep := partialTagTail(p); keep > 0 {
					spoken.WriteString(p[:len(p)-keep])
					pending.Reset()
					pending.WriteString(p[len(p)-keep:])
				} else {
					spoken.WriteString(p)
					pending.Reset()
				}
				return
			}
			spoken.WriteString(p[:start])
			rest := p[start+len("<think>"):]
			pending.Reset()
			pending.WriteString(rest)
			inThink = true
		}
	}

	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("prism read: %w", err)
		}
		var ev struct {
			Type    string                 `json:"type"`
			Content string                 `json:"content"`
			Tool    string                 `json:"tool"`
			Args    map[string]interface{} `json:"args"`
			ID      string                 `json:"id"`
			Input   json.RawMessage        `json:"input"`
			Output  string                 `json:"output"`
			IsError bool                   `json:"is_error"`
		}
		if json.Unmarshal(data, &ev) != nil {
			continue
		}

		switch ev.Type {
		case "tool_use", "tool_result":
			var input map[string]interface{}
			_ = json.Unmarshal(ev.Input, &input)
			observeToolTrace(ctx, ToolTrace{Type: ev.Type, ID: ev.ID, Tool: ev.Tool, Input: input, Output: ev.Output, IsError: ev.IsError})
		case "stream":
			pending.WriteString(ev.Content)
			classify()
			if !firstSpokenLogged && strings.TrimSpace(spoken.String()) != "" {
				firstSpokenLogged = true
				log.Printf("prism: latency: connect=%dms first_spoken=%dms", connectedAt.Sub(requestStarted).Milliseconds(), time.Since(requestStarted).Milliseconds())
			}
			if err := flushSentences(); err != nil {
				return err
			}

		case "telephony":
			// Prism decided to transfer / take a message / hang up — only Vox can
			// do it. Collect the call and surface it on toolsCh at end of turn, like
			// a local tool call, so the handler runs its existing transfer/hangup path.
			if ev.Tool != "" {
				privacy.Logf("prism: telephony tool relayed — %s %v", ev.Tool, ev.Args)
				relayed = append(relayed, ToolCall{Name: ev.Tool, Arguments: ev.Args, Relayed: true})
			}

		case "error":
			return fmt.Errorf("prism error: %s", ev.Content)

		case "turn_complete":
			c.GatewayContext = nil
			// Flush whatever is left (a final fragment with no end punctuation).
			if !inThink {
				spoken.WriteString(pending.String())
			}
			pending.Reset()
			if tail := sanitizeForSpeech(spoken.String()); tail != "" {
				nSent++
				select {
				case sentCh <- tail:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			if len(relayed) > 0 {
				select {
				case toolsCh <- relayed:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			log.Printf("prism: turn done — %d sentence(s), %d telephony tool(s)", nSent, len(relayed))
			return nil
		}
	}
}

func prismSocketURL(base string, query neturl.Values) (string, error) {
	u, err := neturl.Parse(base)
	if err != nil {
		return "", fmt.Errorf("prism URL: %w", err)
	}
	switch u.Scheme {
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	case "ws", "wss":
	default:
		return "", fmt.Errorf("prism URL must use http, https, ws or wss")
	}
	if u.Host == "" || u.User != nil {
		return "", fmt.Errorf("prism URL requires a host without embedded credentials")
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/ws"
	u.RawPath = ""
	q := u.Query()
	for key, values := range query {
		q[key] = values
	}
	u.RawQuery, u.Fragment = q.Encode(), ""
	return u.String(), nil
}

// partialTagTail returns how many trailing bytes of s could be the start of a
// "<think>" tag split across chunks, so we don't speak "<thi" and then swallow
// the rest of the tag.
func partialTagTail(s string) int {
	return partialMarkerTail(s, "<think>")
}

func partialMarkerTail(s, tag string) int {
	max := len(tag) - 1
	if len(s) < max {
		max = len(s)
	}
	for n := max; n > 0; n-- {
		if strings.HasPrefix(tag, s[len(s)-n:]) {
			return n
		}
	}
	return 0
}

// ─── Speech sanitisation ────────────────────────────────────────────────────
// Prism writes for a dashboard; the phone hears a voice. Strip everything a TTS
// would either mispronounce or read aloud as punctuation noise.

var (
	mdLinkRE   = regexp.MustCompile(`\[([^\]]*)\]\([^)]*\)`) // [texte](url) → texte
	mdBulletRE = regexp.MustCompile(`(?m)^\s*[-*+]\s+`)      // puces
	mdHeadRE   = regexp.MustCompile(`(?m)^\s*#{1,6}\s*`)     // titres
	mdEmphRE   = regexp.MustCompile("[*_`~]+")               // **gras**, `code`, ~~barré~~
)

// sanitizeForSpeech makes Prism's screen-oriented text safe to speak.
func sanitizeForSpeech(s string) string {
	s = mdLinkRE.ReplaceAllString(s, "$1")
	s = mdBulletRE.ReplaceAllString(s, "")
	s = mdHeadRE.ReplaceAllString(s, "")
	s = mdEmphRE.ReplaceAllString(s, "")
	s = stripEmoji(s)
	return strings.TrimSpace(strings.Join(strings.Fields(s), " "))
}

// stripEmoji drops pictographs, dingbats and their joiners/variation selectors.
// Without this the TTS voices them (or spells their name) mid-sentence.
func stripEmoji(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 0x1F000 && r <= 0x1FAFF: // emoji, pictographs, flags
			return -1
		case r >= 0x2600 && r <= 0x27BF: // misc symbols + dingbats (✅ ✨ ➡)
			return -1
		case r >= 0x2B00 && r <= 0x2BFF: // arrows/stars
			return -1
		case r == 0xFE0F || r == 0xFE0E: // variation selectors
			return -1
		case r == 0x200D: // zero-width joiner
			return -1
		}
		return r
	}, s)
}
