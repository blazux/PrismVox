package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestPrismRESTAcceptsWebSocketBaseAndPathPrefix(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/prefix/api/voice/caller" || r.Header.Get("Authorization") != "Bearer fixture" {
			t.Error("lost prefix or service credential")
		}
		w.Write([]byte(`{"known":true,"name":"Fixture"}`))
	}))
	defer srv.Close()
	c := &Client{PrismURL: "ws" + strings.TrimPrefix(srv.URL, "http") + "/prefix/", PrismToken: "fixture"}
	if !c.LookupCaller(context.Background(), "+12025550199").Known {
		t.Fatal("WS base failed caller lookup")
	}
	got, err := prismHTTPURL("wss://example.test/prefix/", "/api/voice/search")
	if err != nil || got != "https://example.test/prefix/api/voice/search" {
		t.Fatalf("WSS base: %q %v", got, err)
	}
}

func TestPrismRESTCannotForwardServiceTokenToAnotherOrigin(t *testing.T) {
	var reached atomic.Bool
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached.Store(true)
	}))
	defer foreign.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, foreign.URL, http.StatusTemporaryRedirect)
	}))
	defer srv.Close()
	c := &Client{PrismURL: srv.URL, PrismToken: "fixture"}
	if c.LookupCaller(context.Background(), "+12025550199").Known || reached.Load() {
		t.Fatal("cross-origin redirect followed")
	}
}

// Docked is the routing rule for a whole call. Splitting its two halves by
// accident is how every stranger ends up on the reasoning model — or how a
// colleague loses their own agent — so it is pinned here.
func TestDockedNeedsBothHalves(t *testing.T) {
	for _, tc := range []struct {
		name  string
		url   string
		known bool
		want  bool
	}{
		{"wired and recognised", "http://prism:8080", true, true},
		{"wired, stranger on the line", "http://prism:8080", false, false},
		{"undocked Vox, lookup somehow set", "", true, false},
		{"undocked and unknown", "", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &Client{PrismURL: tc.url, CallerKnown: tc.known}
			if got := c.Docked(); got != tc.want {
				t.Fatalf("Docked() = %v, want %v", got, tc.want)
			}
		})
	}
}

// A caller Prism recognises must come back with their name — that name is spoken
// before any model runs.
func TestLookupCallerKnown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("number"); got != "+33612345678" {
			t.Errorf("number = %q, want the caller's", got)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer s3cret" {
			t.Errorf("Authorization = %q, want the service token", got)
		}
		w.Write([]byte(`{"known":true,"name":"Vincent"}`))
	}))
	defer srv.Close()

	c := &Client{PrismURL: srv.URL, PrismToken: "s3cret"}
	got := c.LookupCaller(context.Background(), "+33612345678")
	if !got.Known || got.Name != "Vincent" {
		t.Fatalf("LookupCaller() = %+v, want known Vincent", got)
	}
}

// Every way the lookup can go wrong must land on "unknown" — that is what keeps
// the phone answering when Prism is down. Failing closed here would mean a dead
// line, not a degraded one.
func TestLookupCallerDegradesToUnknown(t *testing.T) {
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer broken.Close()
	garbage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`not json at all`))
	}))
	defer garbage.Close()

	for _, tc := range []struct{ name, url, number string }{
		{"Prism returns 500", broken.URL, "+33612345678"},
		{"Prism returns nonsense", garbage.URL, "+33612345678"},
		{"Prism unreachable", "http://127.0.0.1:1", "+33612345678"},
		{"Vox is not docked", "", "+33612345678"},
		{"no caller ID at all", broken.URL, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &Client{PrismURL: tc.url}
			if got := c.LookupCaller(context.Background(), tc.number); got.Known {
				t.Fatalf("LookupCaller() = %+v, want unknown", got)
			}
		})
	}
}

// A knowledge lookup happens while a caller waits in silence. Every failure must
// come back as an error the call code can turn into "je n'ai pas cette
// information" — never a hang, never a partial answer presented as fact.
func TestSearchKnowledgeFailsCleanly(t *testing.T) {
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer broken.Close()

	for _, tc := range []struct{ name, url string }{
		{"Prism errors", broken.URL},
		{"Prism unreachable", "http://127.0.0.1:1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &Client{PrismURL: tc.url}
			hits, err := c.SearchKnowledge(context.Background(), "horaires d'ouverture")
			if err == nil {
				t.Fatal("a failed search must report an error, not an empty result that reads as 'we know nothing'")
			}
			if hits != nil {
				t.Fatalf("hits = %v, want none", hits)
			}
		})
	}

	// Undocked Vox: no Prism to ask, and that is not an error — the local
	// pgvector answers instead.
	c := &Client{}
	if hits, err := c.SearchKnowledge(context.Background(), "x"); err != nil || hits != nil {
		t.Fatalf("undocked search = (%v, %v), want (nil, nil)", hits, err)
	}
}

// The question must travel as text: the query vector has to come from the model
// that indexed the corpus, and Vox's embedder is not it.
func TestSearchKnowledgeSendsText(t *testing.T) {
	var got map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&got)
		w.Write([]byte(`{"hits":[{"content":"Ouvert de 9h à 18h","source":"horaires.md","score":0.9}]}`))
	}))
	defer srv.Close()

	c := &Client{PrismURL: srv.URL, PrismToken: "t"}
	hits, err := c.SearchKnowledge(context.Background(), "vous ouvrez à quelle heure ?")
	if err != nil {
		t.Fatalf("search failed: %v", err)
	}
	if got["query"] != "vous ouvrez à quelle heure ?" {
		t.Fatalf("sent %v, want the question as text", got)
	}
	if len(hits) != 1 || hits[0].Source != "horaires.md" {
		t.Fatalf("hits = %+v", hits)
	}
}
