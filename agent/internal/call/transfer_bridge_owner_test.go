package call

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"phoneagent/internal/ari"
	"phoneagent/internal/config"
)

func TestFinalBridgeIsRegisteredBeforeAddingChannelsAndOutlivesCleanup(t *testing.T) {
	var mu sync.Mutex
	var bridgeID, caller, recipient string
	deletedFinal := false
	h := NewHandler("caller", nil, nil, "")
	h.SetTransferBridgeCallbacks(func(id, from, to string) { mu.Lock(); bridgeID, caller, recipient = id, from, to; mu.Unlock() }, func(string) { t.Error("successful bridge was abandoned") })
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/bridges":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"final"}`))
		case r.URL.Path == "/bridges/final/addChannel":
			mu.Lock()
			registered := bridgeID == "final" && caller == "caller" && recipient == "recipient"
			mu.Unlock()
			if !registered {
				t.Error("bridge had no independent owner before adding phone channels")
			}
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodDelete && r.URL.Path == "/bridges/final":
			mu.Lock()
			deletedFinal = true
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer server.Close()
	h.ari = ari.New(server.URL, "", "")
	if err := h.bridgeTransfer("recipient"); err != nil {
		t.Fatal(err)
	}
	h.cleanup(context.Background())
	mu.Lock()
	defer mu.Unlock()
	if bridgeID != "final" || deletedFinal {
		t.Fatalf("handler cleanup destroyed or lost the final bridge: id=%q deleted=%v", bridgeID, deletedFinal)
	}
}

func TestFailedBridgeReturnsItsOwnershipForCleanup(t *testing.T) {
	h := NewHandler("caller", nil, nil, "")
	var mu sync.Mutex
	created, abandoned := "", ""
	h.SetTransferBridgeCallbacks(func(id, _, _ string) { mu.Lock(); created = id; mu.Unlock() }, func(id string) { mu.Lock(); abandoned = id; mu.Unlock() })
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/bridges" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"failed"}`))
			return
		}
		w.WriteHeader(http.StatusConflict)
	}))
	defer server.Close()
	h.ari = ari.New(server.URL, "", "")
	if err := h.bridgeTransfer("recipient"); err == nil {
		t.Fatal("rejected bridge was accepted")
	}
	mu.Lock()
	defer mu.Unlock()
	if created != "failed" || abandoned != created {
		t.Fatalf("failed bridge owner was not notified: created=%q abandoned=%q", created, abandoned)
	}
}

func TestRedirectedHandoffDoesNotReleaseCallerOwnership(t *testing.T) {
	for _, kind := range []string{"bridge", "refer", "attended"} {
		t.Run(kind, func(t *testing.T) {
			var mu sync.Mutex
			callerDeletes := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodDelete {
					if r.URL.Path == "/channels/caller" {
						mu.Lock()
						callerDeletes++
						mu.Unlock()
					}
					w.WriteHeader(http.StatusNoContent)
					return
				}
				if r.URL.Path == "/bridges" {
					_, _ = w.Write([]byte(`{"id":"redirected"}`))
					return
				}
				w.Header().Set("Location", "/not-a-handoff")
				w.WriteHeader(http.StatusFound)
			}))
			defer server.Close()
			h := NewHandler("caller", ari.New(server.URL, "", ""), nil, "")
			h.bridgeID = "media"
			var err error
			if kind == "attended" {
				err = h.bridgeTransfer("recipient")
			} else {
				err = h.executeTransfer(context.Background(), config.FromMap(map[string]string{"sip_transfer_method": kind}), "101")
			}
			if err == nil {
				t.Fatal("redirect was accepted as a handoff")
			}
			h.cleanup(context.Background())
			mu.Lock()
			defer mu.Unlock()
			if callerDeletes != 1 {
				t.Fatalf("redirect released caller ownership: caller deletes=%d", callerDeletes)
			}
		})
	}
}
