package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"phoneagent/internal/ari"
	"phoneagent/internal/call"
)

func TestFinalBridgeWaitsForBothChannelsAndCleansOnce(t *testing.T) {
	var deletes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			deletes.Add(1)
			time.Sleep(5 * time.Millisecond)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	client := ari.New(server.URL, "", "")
	c := newCoordinator()
	c.trackTransferBridge("final", "caller", "recipient")
	c.transferChannelEnded("caller")
	c.cleanupTransferBridges(client)
	if deletes.Load() != 0 {
		t.Fatal("bridge destroyed while recipient was still present")
	}
	c.transferChannelEnded("recipient")
	var group sync.WaitGroup
	for i := 0; i < 20; i++ {
		group.Add(1)
		go func() { defer group.Done(); c.cleanupTransferBridges(client) }()
	}
	group.Wait()
	c.cleanupTransferBridges(client)
	if deletes.Load() != 1 || len(c.transferBridgeSnapshot()) != 0 {
		t.Fatalf("bridge cleanup was not idempotent: deletes=%d tracked=%d", deletes.Load(), len(c.transferBridgeSnapshot()))
	}
}

func TestFinalBridgeReconcilesMissedHangupEvents(t *testing.T) {
	var mu sync.Mutex
	live := []map[string]string{{"id": "recipient"}}
	deletes := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Method == http.MethodGet && r.URL.Path == "/channels" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(live)
			return
		}
		if r.Method == http.MethodDelete {
			deletes++
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	client := ari.New(server.URL, "", "")
	c := newCoordinator()
	c.trackTransferBridge("final", "caller", "recipient")
	c.syncTransferBridges(client)
	mu.Lock()
	first := deletes
	live = nil
	mu.Unlock()
	if first != 0 {
		t.Fatal("reconciliation destroyed a bridge with a live recipient")
	}
	c.syncTransferBridges(client)
	mu.Lock()
	count := deletes
	mu.Unlock()
	if count != 1 || len(c.transferBridgeSnapshot()) != 0 {
		t.Fatalf("missed end events leaked bridge: deletes=%d tracked=%d", count, len(c.transferBridgeSnapshot()))
	}
}

func TestBridgeRegisteredDuringInventoryWaitsForFreshSnapshot(t *testing.T) {
	inventory, release := make(chan struct{}), make(chan struct{})
	var newDeletes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/channels" {
			close(inventory)
			<-release
			_, _ = w.Write([]byte(`[]`))
			return
		}
		if r.Method == http.MethodDelete && r.URL.Path == "/bridges/new" {
			newDeletes.Add(1)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	client := ari.New(server.URL, "", "")
	c := newCoordinator()
	c.trackTransferBridge("old", "old-caller", "old-recipient")
	done := make(chan struct{})
	go func() { defer close(done); c.syncTransferBridges(client) }()
	<-inventory
	c.trackTransferBridge("new", "caller", "recipient")
	close(release)
	<-done
	// The old HTTP snapshot knew neither new phone channel. Ending only the caller
	// must still leave the recipient protecting the newly registered bridge.
	c.transferChannelEnded("caller")
	c.cleanupTransferBridges(client)
	if newDeletes.Load() != 0 || len(c.transferBridgeSnapshot()) != 1 {
		t.Fatalf("stale channel inventory retired a new bridge: deletes=%d tracked=%d", newDeletes.Load(), len(c.transferBridgeSnapshot()))
	}
}

func TestFinalBridgeFailedDeleteKeepsOwnerUntilConfirmedGone(t *testing.T) {
	var fail atomic.Bool
	fail.Store(true)
	var deletes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/channels" {
			_, _ = w.Write([]byte(`[]`))
			return
		}
		if r.Method == http.MethodDelete {
			deletes.Add(1)
		}
		if fail.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	client := ari.New(server.URL, "", "")
	c := newCoordinator()
	c.trackTransferBridge("final", "caller", "recipient")
	c.syncTransferBridges(client)
	if len(c.transferBridgeSnapshot()) != 1 {
		t.Fatal("unconfirmed bridge deletion lost its cleanup owner")
	}
	fail.Store(false)
	c.syncTransferBridges(client)
	if deletes.Load() != 2 || len(c.transferBridgeSnapshot()) != 0 {
		t.Fatalf("bridge deletion was not retried: deletes=%d tracked=%d", deletes.Load(), len(c.transferBridgeSnapshot()))
	}
}

func TestAbortedBridgeKeepsCleanupOwnerAfterARIFailure(t *testing.T) {
	c := newCoordinator()
	c.trackTransferBridge("failed", "caller", "recipient")
	c.abandonTransferBridge("failed")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }))
	defer server.Close()
	c.cleanupTransferBridges(ari.New(server.URL, "", ""))
	if len(c.transferBridgeSnapshot()) != 1 {
		t.Fatal("aborted bridge was forgotten while ARI was unavailable")
	}
}

func TestChannelDestroyedDispatchCleansFinalBridge(t *testing.T) {
	deleted := make(chan struct{}, 1)
	rest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete && r.URL.Path == "/bridges/final" {
			deleted <- struct{}{}
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer rest.Close()
	upgrader := websocket.Upgrader{}
	events := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for _, id := range []string{"caller", "recipient"} {
			_ = conn.WriteJSON(map[string]any{"type": "ChannelDestroyed", "channel": map[string]string{"id": id}})
		}
	}))
	defer events.Close()
	c := newCoordinator()
	c.trackTransferBridge("final", "caller", "recipient")
	_ = connectAndServe(context.Background(), "ws"+strings.TrimPrefix(events.URL, "http"), "", "", ari.New(rest.URL, "", ""), nil, c, map[string]*call.Handler{}, map[string]string{})
	select {
	case <-deleted:
	case <-time.After(time.Second):
		t.Fatal("ChannelDestroyed did not retire final bridge")
	}
}
