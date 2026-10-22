package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func reviewSSEServer(parts []map[string]interface{}) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, delta := range parts {
			raw, _ := json.Marshal(map[string]interface{}{"choices": []interface{}{map[string]interface{}{"delta": delta}}})
			fmt.Fprintf(w, "data: %s\n\n", raw)
			w.(http.Flusher).Flush()
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
}

func TestFragmentedLocalThinkingNeverReachesSpeech(t *testing.T) {
	s := reviewSSEServer([]map[string]interface{}{{"content": "<thi"}, {"content": "nk>REVIEW_INTERNAL_THOUGHT. "}, {"content": "</think>Bonjour."}})
	defer s.Close()
	c := &Client{URL: s.URL, Model: "fixture", http: s.Client(), streamHTTP: s.Client()}
	sent, tools := make(chan string, 16), make(chan []ToolCall, 1)
	if err := c.streamInner(context.Background(), "", nil, "", "", nil, "Synthetic request", sent, tools); err != nil {
		t.Fatal(err)
	}
	var spoken []string
	for len(sent) > 0 {
		spoken = append(spoken, <-sent)
	}
	if strings.Join(spoken, " ") != "Bonjour." {
		t.Fatal("reasoning reached spoken output or answer was lost")
	}
}

func TestRoutineLogsExcludeToolArguments(t *testing.T) {
	t.Setenv("LOG_SENSITIVE_DATA", "false")
	var logs bytes.Buffer
	old := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(old)
	s := reviewSSEServer([]map[string]interface{}{{"tool_calls": []interface{}{map[string]interface{}{"index": 0, "id": "fixture", "function": map[string]interface{}{"name": "take_message", "arguments": `{"content":"review-synthetic-private"}`}}}}})
	defer s.Close()
	c := &Client{URL: s.URL, Model: "fixture", http: s.Client(), streamHTTP: s.Client()}
	if err := c.streamInner(context.Background(), "", nil, "", "", nil, "Synthetic request", make(chan string, 16), make(chan []ToolCall, 1)); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(logs.String(), "review-synthetic-private") {
		t.Fatal("private tool argument appeared in routine logs")
	}
}

func TestReasoningMarkersAtEveryChunkBoundary(t *testing.T) {
	raw := "<think>Do not speak this. </think>Bonjour. <think>Nor this.</think>Au revoir."
	for width := 1; width <= len(raw); width++ {
		var f speechFilter
		var spoken strings.Builder
		for pos := 0; pos < len(raw); pos += width {
			end := pos + width
			if end > len(raw) {
				end = len(raw)
			}
			spoken.WriteString(f.Feed(raw[pos:end]))
		}
		if spoken.String() != "Bonjour. Au revoir." {
			t.Fatalf("chunk width=%d: %q", width, spoken.String())
		}
	}
	var f speechFilter
	if got := f.Feed("Hidden reasoning.</think>Bonjour."); got != "Bonjour." {
		t.Fatal(got)
	}
}
