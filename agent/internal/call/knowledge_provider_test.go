package call

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"phoneagent/internal/llm"
)

func TestLinkedKnowledgeDoesNotRequireLocalEmbeddingProvider(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request map[string]string
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		if r.URL.Path != "/api/voice/search" || request["query"] != "opening hours" {
			t.Error("connected lookup did not send the question to Prism")
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"hits":[{"content":"Open at 9","source":"hours.txt"}]}`))
	}))
	defer server.Close()
	// No local database or embedding client: connected calls must use Prism's
	// text search and its own index rather than the standalone provider.
	h := &Handler{llmClient: &llm.Client{PrismURL: server.URL}}
	result := h.searchKnowledge(context.Background(), "opening hours")
	if !strings.Contains(result, "Open at 9") || !strings.Contains(result, "hours.txt") {
		t.Fatal("Prism knowledge was unavailable without the retired service", result)
	}
}
