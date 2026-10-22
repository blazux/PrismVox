package llm

// What Prism knows about people, asked over HTTP.
//
// Vox has to settle one question before it even answers the line: is this number
// one of ours? The answer decides which brain takes the call and which greeting is
// spoken — and it only exists in Prism, whose user profiles carry the numbers.
//
// Everything here degrades to "unknown" rather than failing. A Prism that is down,
// slow or undocked must not stop the phone from ringing: the switchboard answers
// on its own brain, which is exactly what it does for a stranger.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	neturl "net/url"
	"strings"
	"time"
)

// identityTimeout bounds the identity lookups. The caller is already on the line
// and the greeting is waiting on the answer, so this is deliberately short: past a
// couple of seconds, answering as the switchboard beats answering late.
const identityTimeout = 2 * time.Second

// Use the same base address for REST and WebSocket docking, including a prefix.
// ws/wss spellings accepted by the streaming client must work for lookup too.
func prismHTTPURL(base, path string) (string, error) {
	u, err := neturl.Parse(base)
	if err != nil {
		return "", fmt.Errorf("invalid Prism URL")
	}
	switch u.Scheme {
	case "ws":
		u.Scheme = "http"
	case "wss":
		u.Scheme = "https"
	case "http", "https":
	default:
		return "", fmt.Errorf("Prism URL must use HTTP(S) or WS(S)")
	}
	if u.Host == "" || u.User != nil {
		return "", fmt.Errorf("Prism URL requires a host without embedded credentials")
	}
	route, err := neturl.Parse(path)
	if err != nil {
		return "", err
	}
	u.Path = strings.TrimRight(u.Path, "/") + route.Path
	u.RawPath, u.Fragment = "", ""
	q := u.Query()
	for key, values := range route.Query() {
		q[key] = values
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

var prismHTTP = &http.Client{CheckRedirect: func(req *http.Request, via []*http.Request) error {
	if len(via) >= 5 || req.URL.Scheme != via[0].URL.Scheme || req.URL.Host != via[0].URL.Host {
		return fmt.Errorf("Prism redirect outside configured origin refused")
	}
	return nil
}}

// Caller is what Prism knows about the number on the line.
type Caller struct {
	// Known reports whether the number matches an approved Prism user. It is not
	// authentication — caller ID is trivially forged. It selects a greeting and a
	// brain, never a permission.
	Known bool `json:"known"`
	// Name is the display name to greet them with. Empty when unknown.
	Name string `json:"name"`
}

// DirEntry is one person a call can be transferred to.
type DirEntry struct {
	Name  string `json:"name"`
	Phone string `json:"phone"`
	// Transfer is how they want to be handed a call: "blind" straight through, or
	// "attended" announced first so they can decline. Each person sets it on their
	// own Prism profile — Vox holds the machinery, not the preference.
	Transfer string `json:"transfer"`
}

// LookupCaller asks Prism who is calling. An undocked Vox, an unreachable Prism or
// any error at all answers "unknown" — the switchboard then takes the call itself.
func (c *Client) LookupCaller(ctx context.Context, number string) Caller {
	if c.PrismURL == "" || number == "" {
		return Caller{}
	}
	var out Caller
	q := neturl.Values{}
	q.Set("number", number)
	if err := c.getPrismJSON(ctx, "/api/voice/caller?"+q.Encode(), &out); err != nil {
		log.Printf("llm: caller lookup failed (%v) — treating the call as unknown", err)
		return Caller{}
	}
	return out
}

// TransferDirectory lists the people a call can be transferred to: approved Prism
// users with a phone number. Vox's own contacts table is the OUTBOUND directory —
// people it calls — and the two must never be merged, or the switchboard offers to
// put a caller through to a supplier.
func (c *Client) TransferDirectory(ctx context.Context) ([]DirEntry, error) {
	if c.PrismURL == "" {
		return nil, nil
	}
	var out struct {
		Entries []DirEntry `json:"entries"`
	}
	if err := c.getPrismJSON(ctx, "/api/voice/directory", &out); err != nil {
		return nil, err
	}
	return out.Entries, nil
}

// getPrismJSON performs one short, authenticated GET against Prism and decodes the
// body. Same service token as the docking WebSocket.
func (c *Client) getPrismJSON(ctx context.Context, path string, out interface{}) error {
	ctx, cancel := context.WithTimeout(ctx, identityTimeout)
	defer cancel()

	address, err := prismHTTPURL(c.PrismURL, path)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, "GET", address, nil)
	if err != nil {
		return err
	}
	if c.PrismToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.PrismToken)
	}
	resp, err := prismHTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: HTTP %d", path, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// searchTimeout bounds a knowledge lookup. Longer than the identity lookups
// because a vector search is real work, but still short: the caller is waiting in
// silence while it runs, and an answer that arrives late is worse than an honest
// "je n'ai pas cette information".
const searchTimeout = 5 * time.Second

// KBHit is one passage from Prism's switchboard knowledge base.
type KBHit struct {
	Content string  `json:"content"`
	Source  string  `json:"source"`
	Score   float64 `json:"score"`
}

// SearchKnowledge asks Prism its switchboard knowledge base.
//
// The question travels as TEXT and Prism embeds it. That is the whole point: the
// query vector has to come from the model that indexed the corpus, and Vox's own
// embedder is a different model describing a different space — it would return
// confident nonsense rather than nothing, which is worse.
func (c *Client) SearchKnowledge(ctx context.Context, query string) ([]KBHit, error) {
	if c.PrismURL == "" {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, searchTimeout)
	defer cancel()

	body, err := json.Marshal(map[string]string{"query": query})
	if err != nil {
		return nil, err
	}
	address, err := prismHTTPURL(c.PrismURL, "/api/voice/search")
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", address, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.PrismToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.PrismToken)
	}
	resp, err := prismHTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("knowledge search: HTTP %d", resp.StatusCode)
	}
	var out struct {
		Hits []KBHit `json:"hits"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out.Hits, nil
}
