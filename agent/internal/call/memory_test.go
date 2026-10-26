package call

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"phoneagent/internal/ari"
	"phoneagent/internal/config"
	"phoneagent/internal/llm"
)

func consumeCompaction(t *testing.T, h *Handler) {
	t.Helper()
	select {
	case result := <-h.memoryPending:
		h.memoryPending <- result
	case <-time.After(2 * time.Second):
		t.Fatal("compaction did not finish")
	}
	h.compactHistory(context.Background())
}

func TestLongCallCompactionPreservesFactsAndNewTurns(t *testing.T) {
	requests := make(chan string, 3)
	gate := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		requests <- fmt.Sprint(payload["messages"])
		<-gate
		fmt.Fprint(w, `{"choices":[{"message":{"content":"Gérard demande un devis pour vendredi, pas jeudi."}}]}`)
	}))
	defer srv.Close()
	t.Setenv("OPENAI_BASE_URL", srv.URL)
	h := NewHandler("fixture", nil, nil, "")
	for i := 0; i < 22; i++ {
		text := fmt.Sprintf("caller-%d", i)
		if i == 0 {
			text = "Je m'appelle Gérard, devis vendredi et pas jeudi"
		}
		h.history = append(h.history, llm.Message{Role: "user", Content: text}, llm.Message{Role: "assistant", Content: "noté"})
		h.turns = append(h.turns, map[string]string{"role": "caller", "text": text}, map[string]string{"role": "agent", "text": "noté"})
	}
	h.compactHistory(context.Background())
	// The call loop returned while the provider is still blocked.
	if len(h.history) != 44 {
		t.Fatal("history removed before summary completed")
	}
	input := <-requests
	if !strings.Contains(input, "Gérard") {
		t.Fatal("older caller facts missing from compaction input")
	}
	h.history = append(h.history, llm.Message{Role: "user", Content: "late turn"}, llm.Message{Role: "assistant", Content: "late reply"})
	close(gate)
	consumeCompaction(t, h)
	if len(h.history) != 22 || h.history[len(h.history)-1].Content != "late reply" {
		t.Fatal("new turns lost during compaction")
	}
	if !strings.Contains(h.systemPrompt(&config.Config{}), "Gérard") {
		t.Fatal("summary missing from next model prompt")
	}
	if len(h.callTranscript()) != 44 || !callerSaidName("Gérard", userTurns(h.callTranscript())) {
		t.Fatal("compaction changed the source transcript")
	}
	// A subsequent compaction must consolidate, not forget, the earlier summary.
	for i := 0; i < 22; i++ {
		h.history = append(h.history, llm.Message{Role: "user", Content: "more"}, llm.Message{Role: "assistant", Content: "ok"})
	}
	h.compactHistory(context.Background())
	if input = <-requests; !strings.Contains(input, "Gérard demande un devis") {
		t.Fatal("previous summary not consolidated")
	}
	consumeCompaction(t, h)
}

func TestCompactionFailureKeepsRawContext(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "unavailable", 503) }))
	defer srv.Close()
	t.Setenv("OPENAI_BASE_URL", srv.URL)
	h := NewHandler("fixture", nil, nil, "")
	h.historySummary = "previous facts"
	for i := 0; i < 42; i++ {
		h.history = append(h.history, llm.Message{Role: "user", Content: fmt.Sprint(i)})
	}
	h.compactHistory(context.Background())
	consumeCompaction(t, h)
	if len(h.history) != 42 || h.historySummary != "previous facts" || h.memoryPending != nil {
		t.Fatal("failure discarded context or immediately retried")
	}
}

func TestCancelledCallCannotPopulateAnotherCallsMemory(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer srv.Close()
	defer close(release)
	t.Setenv("OPENAI_BASE_URL", srv.URL)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first := NewHandler("first", nil, nil, "")
	for i := 0; i < 42; i++ {
		first.history = append(first.history, llm.Message{Role: "user", Content: "first caller private fact"})
	}
	first.compactHistory(ctx)
	<-started
	cancel()
	select {
	case result := <-first.memoryPending:
		if result.err == nil {
			t.Fatal("cancelled provider request succeeded")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled compaction did not stop")
	}
	if len(first.history) != 42 || first.historySummary != "" {
		t.Fatal("cancelled job changed call memory")
	}
	second := NewHandler("second", nil, nil, "")
	if len(second.history) != 0 || second.historySummary != "" || second.memoryPending != nil {
		t.Fatal("new call inherited previous caller context")
	}
}

func TestTransferOutcomeDistinguishesInitiatedAndFailed(t *testing.T) {
	for _, status := range []int{200, 500} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(status) }))
			defer srv.Close()
			h := NewHandler("fixture", ari.New(srv.URL, "", ""), nil, "")
			h.toolsUsed = []map[string]interface{}{{"tool": "transfer_call", "outcome": "queued"}}
			err := h.executeTransfer(context.Background(), &config.Config{}, "123")
			want := "initiated"
			if status == 500 {
				want = "failed"
				if err == nil {
					t.Fatal("failure not propagated")
				}
			}
			if h.toolsUsed[0]["outcome"] != want {
				t.Fatalf("wrong transfer outcome: %v", h.toolsUsed)
			}
		})
	}
}
