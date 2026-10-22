package call

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"phoneagent/internal/ari"
	"phoneagent/internal/config"
	"phoneagent/internal/llm"
)

func TestHangupBeforeRunAndDuplicateEvents(t *testing.T) {
	h := NewHandler("test", nil, nil, "")
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); h.Stop() }()
	}
	wg.Wait()
	go h.Run(context.Background())
	select {
	case <-h.Done():
	case <-time.After(time.Second):
		t.Fatal("stopped call did not finish")
	}
}

func TestCancelledCallDoesNotExecuteTools(t *testing.T) {
	h := NewHandler("test", nil, nil, "")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := h.handleToolCalls(ctx, "", []llm.ToolCall{{Name: "end_call"}}, &config.Config{}, "", "")
	if err == nil || h.endCall {
		t.Fatal("cancelled call executed a phone action")
	}
}

func TestAttendedBridgeFailureRestoresCaller(t *testing.T) {
	requests := make(chan string, 8)
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- r.Method + " " + r.URL.Path
		switch r.URL.Path {
		case "/bridges":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"final"}`))
		case "/bridges/final/addChannel":
			w.WriteHeader(http.StatusConflict)
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer s.Close()
	h := NewHandler("caller", ari.New(s.URL, "", ""), nil, "")
	h.bridgeID = "original"
	if err := h.bridgeTransfer("recipient"); err == nil {
		t.Fatal("bridge failure reported as acceptance")
	}
	want := []string{"POST /bridges", "POST /bridges/final/addChannel", "DELETE /bridges/final", "POST /bridges/original/addChannel"}
	for _, expected := range want {
		select {
		case got := <-requests:
			if got != expected {
				t.Fatalf("got %s, want %s", got, expected)
			}
		default:
			t.Fatalf("missing %s", expected)
		}
	}
	if h.bridgeID != "original" {
		t.Fatal("caller lost original media")
	}
}

func TestRejectedTransferPreservesMedia(t *testing.T) {
	for _, method := range []string{"bridge", "refer"} {
		t.Run(method, func(t *testing.T) {
			var deleted bool
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "DELETE" {
					deleted = true
				}
				w.WriteHeader(http.StatusConflict)
			}))
			defer s.Close()
			h := NewHandler("caller", ari.New(s.URL, "", ""), nil, "")
			h.bridgeID, h.extChanID, h.snoopChanID, h.running = "media", "external", "snoop", true
			err := h.executeTransfer(context.Background(), &config.Config{Raw: map[string]string{"sip_transfer_method": method}}, "101")
			if err == nil {
				t.Fatal("ARI rejection was ignored")
			}
			if deleted || !h.running || h.bridgeID != "media" || h.extChanID != "external" || h.snoopChanID != "snoop" {
				t.Fatal("failed handoff destroyed media")
			}
		})
	}
}
