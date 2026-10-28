package embeddings

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOpenAICompatibleBatchAndIdentity(t *testing.T) {
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Path != "/v1/embeddings" || r.Header.Get("Authorization") != "Bearer fixture-secret" {
			t.Error("incorrect API path or credential")
		}
		var body struct {
			Model  string
			Input  []string
			Format string `json:"encoding_format"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body.Model != "fixture-model" || len(body.Input) != 2 || body.Format != "float" {
			t.Error("incorrect embedding request")
		}
		fmt.Fprint(w, `{"data":[{"index":1,"embedding":[0,1,0]},{"index":0,"embedding":[1,0,0]}]}`)
	}))
	defer server.Close()
	c := New()
	raw := map[string]string{"embedding_url": server.URL + "/v1/", "embedding_model": "fixture-model", "embedding_api_key": "fixture-secret", "embedding_dimensions": "3"}
	c.Configure(raw)
	vectors, err := c.Embed(context.Background(), []string{"first", "second"})
	if err != nil || vectors[0][0] != 1 || vectors[1][1] != 1 || requests != 1 {
		t.Fatalf("batch ordering: %v %v", vectors, err)
	}
	key := c.Key(3)
	raw["embedding_model"] = "other-model" // per-call settings are a snapshot
	if c.Key(3) != key {
		t.Fatal("call settings changed underneath the client")
	}
	c.Configure(raw)
	if c.Key(3) == key || c.Key(4) == c.Key(3) {
		t.Fatal("incompatible models or dimensions share an identity")
	}
	c.Configure(map[string]string{"embedding_url": "http://provider:11434/v1", "embedding_model": "fixture-model"})
	if c.Key(3) != "4f10c2eb9229e251e04942ae271797698bc8f7ac8494fa6ca86e9e6898c7983a" {
		t.Fatalf("cross-language index identity changed: %s", c.Key(3))
	}
}

func TestRejectInvalidResponsesAndDoNotEchoSecrets(t *testing.T) {
	for _, body := range []string{
		`{"data":[]}`,
		`{"data":[{"index":1,"embedding":[1,0]}]}`,
		`{"data":[{"index":0,"embedding":[0,0]}]}`,
		`{"data":[{"index":0,"embedding":[1,0,0]}]}`,
		`{"data":[{"index":0,"embedding":[NaN,0]}]}`,
	} {
		t.Run(body, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) }))
			defer server.Close()
			c := New()
			c.Configure(map[string]string{"embedding_url": server.URL + "/v1", "embedding_model": "test", "embedding_dimensions": "2"})
			if _, err := c.EmbedOne(context.Background(), "hello"); err == nil {
				t.Fatal("invalid vectors accepted")
			}
		})
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "fixture-secret", 401) }))
	defer server.Close()
	c := New()
	c.Configure(map[string]string{"embedding_url": server.URL, "embedding_model": "test", "embedding_api_key": "fixture-secret"})
	_, err := c.EmbedOne(context.Background(), "hello")
	if err == nil || strings.Contains(err.Error(), "fixture-secret") {
		t.Fatal("provider error leaked its body")
	}
	c.Configure(map[string]string{"embedding_url": "http://user:password@provider/v1", "embedding_model": "test"})
	if c.Configured() {
		t.Fatal("embedded URL credentials accepted")
	}
}

func TestNoBundledDefaultAndNoCredentialRedirect(t *testing.T) {
	for _, key := range []string{"EMBEDDING_BASE_URL", "EMBEDDING_MODEL", "EMBEDDING_API_KEY"} {
		t.Setenv(key, "")
	}
	t.Setenv("EMBEDDINGS_URL", "http://legacy:8002")
	c := New()
	if c.Configured() {
		t.Fatal("must not depend on the retired local service")
	}
	if _, err := c.EmbedOne(context.Background(), "hello"); err == nil {
		t.Fatal("missing provider accepted")
	}
	leaked := false
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked = true }))
	defer target.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, 302) }))
	defer origin.Close()
	c.Configure(map[string]string{"embedding_url": origin.URL, "embedding_model": "test", "embedding_api_key": "fixture-secret"})
	if _, err := c.EmbedOne(context.Background(), "hello"); err == nil || leaked {
		t.Fatal("embedding request followed a redirect")
	}
}
