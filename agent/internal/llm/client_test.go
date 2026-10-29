package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPhoneReasoningIsLowWhileSummariesDisableIt(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		kw, _ := body["chat_template_kwargs"].(map[string]interface{})
		wantEffort, wantThinking := "none", false
		if body["stream"] == true {
			wantEffort, wantThinking = "low", true
		}
		if body["reasoning_effort"] != wantEffort || kw["enable_thinking"] != wantThinking {
			http.Error(w, "phone request enabled reasoning", http.StatusBadRequest)
			return
		}
		if body["stream"] == true {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"Private calculation.\"}}]}\n\ndata: {\"choices\":[{\"delta\":{\"content\":\"Bonjour.\"}}]}\n\ndata: [DONE]\n\n")
		} else {
			fmt.Fprint(w, `{"choices":[{"message":{"content":"Résumé de test."}}]}`)
		}
	}))
	defer srv.Close()
	t.Setenv("OPENAI_BASE_URL", srv.URL)
	c := New()
	if _, err := c.GetCallSummary(context.Background(), []Message{{Role: "user", Content: "Bonjour"}}, "fixture", nil, false, "hung_up"); err != nil {
		t.Fatal(err)
	}
	sentences, tools, errors := c.StreamResponse(context.Background(), "Réponds en français.", nil, "", "", nil, "Bonjour")
	var spoken string
	for sentence := range sentences {
		spoken += sentence
	}
	for range tools {
	}
	if err := <-errors; err != nil {
		t.Fatal(err)
	}
	if spoken != "Bonjour." {
		t.Fatalf("unexpected spoken answer: %q", spoken)
	}
}

// Regression: Prism and Vox must agree on what the gateway variable holds.
// OPENAI_BASE_URL is the /v1 root (used verbatim); the deprecated SGLANG_URL
// meant the host root and gets /v1 appended. Getting this wrong produced a
// silent /v1/v1/chat/completions.
func TestOpenAIBaseURLConvention(t *testing.T) {
	for _, tc := range []struct{ name, base, sglang, want string }{
		{"new var is the /v1 root", "http://gw:4000/v1", "", "http://gw:4000/v1"},
		{"trailing slash trimmed", "http://gw:4000/v1/", "", "http://gw:4000/v1"},
		{"new var wins over legacy", "http://gw:4000/v1", "http://old:1/", "http://gw:4000/v1"},
		{"legacy gets /v1 appended", "", "http://gw:4000", "http://gw:4000/v1"},
		{"legacy trailing slash", "", "http://gw:4000/", "http://gw:4000/v1"},
		{"neither set", "", "", "http://llm:30000/v1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("OPENAI_BASE_URL", tc.base)
			t.Setenv("SGLANG_URL", tc.sglang)
			if got := openAIBaseURL(); got != tc.want {
				t.Fatalf("openAIBaseURL() = %q, want %q", got, tc.want)
			}
		})
	}
}
