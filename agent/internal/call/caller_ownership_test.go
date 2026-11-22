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
	"phoneagent/internal/db"
)

// All fixtures stop before speech/model work. Only the loopback fake ARI and
// an ephemeral RTP socket are used; no trunk, provider or audio is involved.
func TestRunReleasesCallerOnInitializationFailure(t *testing.T) {
	t.Setenv("STT_PROVIDER", "service")
	t.Setenv("TTS_PROVIDER", "service")
	t.Setenv("STT_URL", "http://127.0.0.1:1")
	t.Setenv("TTS_URL", "http://127.0.0.1:1")
	t.Setenv("OPENAI_BASE_URL", "http://127.0.0.1:1/v1")
	t.Setenv("PRISM_URL", "")
	t.Setenv("CORTEX_URL", "")
	t.Setenv("WAIT_CUE_DIR", "")
	t.Setenv("WAIT_CUE_WAV", "")
	for _, tc := range []struct {
		name, failure               string
		mediaCreated, bridgeCreated bool
		outbound                    bool
		status                      int
	}{
		{name: "voice configuration", failure: "voice"},
		{name: "answer", failure: "/channels/caller/answer"},
		{name: "external media", failure: "/channels/externalMedia"},
		{name: "media bridge", failure: "/bridges", mediaCreated: true},
		{name: "bridge membership", failure: "/bridges/media/addChannel", mediaCreated: true, bridgeCreated: true},
		{name: "outbound voice configuration", failure: "voice", outbound: true},
		{name: "outbound external media", failure: "/channels/externalMedia", outbound: true},
		{name: "redirected answer", failure: "/channels/caller/answer", status: http.StatusFound},
		{name: "redirected bridge membership", failure: "/bridges/media/addChannel", mediaCreated: true, bridgeCreated: true, status: http.StatusFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.failure == "voice" {
				t.Setenv("STT_PROVIDER", "unsupported-fixture")
			}
			var mu sync.Mutex
			deletes := map[string]int{}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodDelete {
					mu.Lock()
					deletes[r.URL.Path]++
					mu.Unlock()
					w.WriteHeader(http.StatusNoContent)
					return
				}
				if r.URL.Path == tc.failure {
					status := tc.status
					if status == 0 {
						status = http.StatusServiceUnavailable
					}
					w.WriteHeader(status)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/channels/externalMedia":
					_, _ = w.Write([]byte(`{"id":"external"}`))
				case "/bridges":
					_, _ = w.Write([]byte(`{"id":"media"}`))
				default:
					w.WriteHeader(http.StatusNoContent)
				}
			}))
			defer server.Close()
			h := NewHandler("caller", ari.New(server.URL, "", ""), nil, "")
			if tc.outbound {
				h = NewOutboundHandler("caller", ari.New(server.URL, "", ""), nil,
					&db.CallTask{ID: 1, PhoneNumber: "12025550199", Mission: "Synthetic fixture"})
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			h.Run(ctx)
			select {
			case <-h.Done():
			default:
				t.Fatal("failed initialization did not finish")
			}
			h.cleanup(context.Background()) // teardown must remain idempotent
			mu.Lock()
			defer mu.Unlock()
			if deletes["/channels/caller"] != 1 {
				t.Fatalf("caller deletion count=%d, want 1; requests=%v", deletes["/channels/caller"], deletes)
			}
			if tc.mediaCreated && deletes["/channels/external"] != 1 {
				t.Fatalf("external media leaked: %v", deletes)
			}
			if tc.bridgeCreated && deletes["/bridges/media"] != 1 {
				t.Fatalf("media bridge leaked: %v", deletes)
			}
		})
	}
}

func TestCleanupPreservesConfirmedTransfer(t *testing.T) {
	for _, kind := range []string{"bridge", "refer", "attended"} {
		t.Run(kind, func(t *testing.T) {
			var mu sync.Mutex
			deletes := map[string]int{}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodDelete {
					mu.Lock()
					deletes[r.URL.Path]++
					mu.Unlock()
				}
				if r.Method == http.MethodPost && r.URL.Path == "/bridges" {
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"id":"connected"}`))
					return
				}
				w.WriteHeader(http.StatusNoContent)
			}))
			defer server.Close()
			h := NewHandler("caller", ari.New(server.URL, "", ""), nil, "")
			h.bridgeID, h.extChanID, h.snoopChanID = "media", "external", "snoop"
			var err error
			if kind == "attended" {
				err = h.bridgeTransfer("recipient")
			} else {
				err = h.executeTransfer(context.Background(), config.FromMap(map[string]string{"sip_transfer_method": kind, "sip_domain": "fixture.invalid"}), "101")
			}
			if err != nil {
				t.Fatal(err)
			}
			h.cleanup(context.Background())
			h.cleanup(context.Background())
			mu.Lock()
			defer mu.Unlock()
			if deletes["/channels/caller"] != 0 || deletes["/channels/recipient"] != 0 || deletes["/bridges/connected"] != 0 {
				t.Fatalf("cleanup interrupted confirmed transfer: %v", deletes)
			}
			for _, resource := range []string{"/bridges/media", "/channels/external", "/channels/snoop"} {
				if deletes[resource] != 1 {
					t.Fatalf("media deletion count for %s=%d, want 1", resource, deletes[resource])
				}
			}
		})
	}
}

func TestRejectedTransferStillReleasesCallerAtCleanup(t *testing.T) {
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
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"id":"rejected"}`))
					return
				}
				w.WriteHeader(http.StatusConflict)
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
				t.Fatal("rejected handoff was accepted")
			}
			h.cleanup(context.Background())
			h.cleanup(context.Background())
			mu.Lock()
			defer mu.Unlock()
			if callerDeletes != 1 {
				t.Fatalf("caller deletion count=%d, want 1", callerDeletes)
			}
		})
	}
}

func TestHangupConsumesCallerOwnershipOnce(t *testing.T) {
	var mu sync.Mutex
	callerDeletes := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete && r.URL.Path == "/channels/caller" {
			mu.Lock()
			callerDeletes++
			mu.Unlock()
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	h := NewHandler("caller", ari.New(server.URL, "", ""), nil, "")
	var group sync.WaitGroup
	for i := 0; i < 20; i++ {
		group.Add(1)
		go func() { defer group.Done(); h.hangup() }()
	}
	group.Wait()
	h.cleanup(context.Background())
	mu.Lock()
	defer mu.Unlock()
	if callerDeletes != 1 {
		t.Fatalf("caller deletion count=%d, want 1", callerDeletes)
	}
}
