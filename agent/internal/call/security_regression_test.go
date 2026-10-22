package call

import (
	"context"
	"net/http"
	"net/http/httptest"
	"phoneagent/internal/config"
	"phoneagent/internal/llm"
	"phoneagent/internal/tts"
	"sync/atomic"
	"testing"
	"time"
)

// Regression fixtures never access an ARI client, trunk or real provider.
func TestUntrustedDialNumberCannotBypassDirectory(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"content":""}}]}`))
	}))
	defer server.Close()
	t.Setenv("OPENAI_BASE_URL", server.URL)
	h := NewHandler("review", nil, nil, "")
	h.llmClient.PrismURL = ""
	h.llmClient.CallerKnown = false
	number := "12025550199" // reserved fictional phone number
	_, err := h.handleToolCalls(context.Background(), "", []llm.ToolCall{{Name: "transfer_call", Arguments: map[string]interface{}{
		"destination": "Not in any directory", "dial_number": number, "transfer_type": "blind",
	}}}, config.FromMap(nil), "", "")
	if err != nil || h.transferDest != "" {
		t.Fatalf("unlisted transfer accepted or rejection failed: queued=%t err=%v", h.transferDest == number, err)
	}

}

func TestTTSSynthesisHonorsCancelledContext(t *testing.T) {
	var requests atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		time.Sleep(250 * time.Millisecond)
		w.Write([]byte("fixture"))
	}))
	defer s.Close()
	t.Setenv("TTS_URL", s.URL)
	h := &Handler{ttsClient: tts.New()}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	h.synthesizeFiller(ctx, "Synthetic review text", make(chan playChunk, 1))
	elapsed := time.Since(start)
	if requests.Load() != 0 || elapsed > 200*time.Millisecond {
		t.Fatalf("cancelled synthesis sent requests=%d elapsed=%s", requests.Load(), elapsed)
	}

}

func TestAuthenticatedPrismRelayKeepsValidatedTransfer(t *testing.T) {
	h := NewHandler("fixture", nil, nil, "")
	h.llmClient.PrismURL = "ws://fixture.invalid"
	h.llmClient.CallerKnown = true
	_, err := h.handleToolCalls(context.Background(), "", []llm.ToolCall{{Name: "transfer_call", Relayed: true, Arguments: map[string]interface{}{"destination": "Approved contact", "dial_number": "12025550199", "transfer_type": "blind"}}}, config.FromMap(nil), "", "")
	if err != nil || h.transferDest != "12025550199" {
		t.Fatalf("validated Prism relay was refused: %v", err)
	}
}
