package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCallSummaryIncludesCallerAndConfirmedActionOutcomes(t *testing.T) {
	input := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		input <- body.Messages[1].Content
		fmt.Fprint(w, `{"choices":[{"message":{"content":"L'appelant demande un devis. Le transfert a échoué."}}]}`)
	}))
	defer srv.Close()
	t.Setenv("OPENAI_BASE_URL", srv.URL)
	transcript := []Message{{Role: "user", Content: "Un devis pour vendredi"}, {Role: "assistant", Content: "Je vous transfère"}, {Role: "user", Content: "Voici ma référence ancienne 98765"}}
	text, err := New().GetCallSummary(context.Background(), transcript, "fixture", []map[string]interface{}{{"tool": "transfer_call", "outcome": "failed"}}, false, "hung_up")
	if err != nil || text == "" {
		t.Fatalf("summary failed: %s %v", text, err)
	}
	got := <-input
	for _, required := range []string{"user: Un devis pour vendredi", "assistant: Je vous transfère", "ancienne 98765", `"outcome":"failed"`, "hung_up"} {
		if !strings.Contains(got, required) {
			t.Errorf("summary input missing %q", required)
		}
	}
}

func TestCompactionRejectsEmptyOrOversizedSummaries(t *testing.T) {
	for _, text := range []string{"", strings.Repeat("x", 2001)} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			json.NewEncoder(w).Encode(map[string]interface{}{"choices": []interface{}{map[string]interface{}{"message": map[string]string{"content": text}}}})
		}))
		t.Setenv("OPENAI_BASE_URL", srv.URL)
		_, err := New().CompactConversation(context.Background(), "", []Message{{Role: "user", Content: "important fact"}})
		srv.Close()
		if err == nil {
			t.Fatal("invalid summary accepted")
		}
	}
}
