package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
	"phoneagent/internal/ari"
	"phoneagent/internal/call"
	"phoneagent/internal/db"
)

func TestUnansweredOutboundRetriesOnlyWithUninterruptedEvents(t *testing.T) {
	for _, kind := range []string{"connected", "disconnected", "reconnected", "started offline"} {
		t.Run(kind, func(t *testing.T) {
			pool := outboundTestPool(t)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) }))
			defer server.Close()
			c := newCoordinator()
			if kind != "started offline" {
				c.setEventsAvailable(true)
			}
			pending := &outboundPending{task: &db.CallTask{ID: 1}, answered: make(chan *call.Handler, 1)}
			c.addPending("outbound-fixture", pending)
			if kind == "disconnected" || kind == "reconnected" {
				c.setEventsAvailable(false)
			}
			if kind == "reconnected" || kind == "started offline" {
				c.setEventsAvailable(true)
			}
			finishUnansweredOutbound(context.Background(), ari.New(server.URL, "", ""), pool, c, "outbound-fixture", pending)
			var status string
			if err := pool.QueryRow(context.Background(), "SELECT status FROM call_tasks WHERE id=1").Scan(&status); err != nil {
				t.Fatal(err)
			}
			expected := "failed"
			if kind == "connected" {
				expected = "pending"
			}
			if status != expected {
				t.Fatalf("events=%s produced %s, want %s", kind, status, expected)
			}
		})
	}
}

// Independent review found that pending has already been removed while DELETE
// blocks. A full disconnect/reconnect during that request must still be retained.
func TestOutageDuringClosureMustNotAutomaticallyRetry(t *testing.T) {
	pool := outboundTestPool(t)
	deleting, release := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			close(deleting)
			<-release
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()
	c := newCoordinator()
	c.setEventsAvailable(true)
	pending := &outboundPending{task: &db.CallTask{ID: 1}, answered: make(chan *call.Handler, 1)}
	c.addPending("outbound-fixture", pending)
	done := make(chan struct{})
	go func() {
		defer close(done)
		finishUnansweredOutbound(context.Background(), ari.New(server.URL, "", ""), pool, c, "outbound-fixture", pending)
	}()
	<-deleting
	c.setEventsAvailable(false)
	c.setEventsAvailable(true)
	close(release)
	<-done
	var status string
	if err := pool.QueryRow(context.Background(), "SELECT status FROM call_tasks WHERE id=1").Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "failed" {
		t.Fatalf("ARI outage during closure was forgotten: status=%s", status)
	}
}

func TestEventSocketLossMarksPendingUncertain(t *testing.T) {
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err == nil {
			conn.Close()
		}
	}))
	defer server.Close()
	c := newCoordinator()
	c.setEventsAvailable(true)
	pending := &outboundPending{task: &db.CallTask{ID: 1}, answered: make(chan *call.Handler, 1)}
	c.addPending("outbound-fixture", pending)
	if err := connectAndServe(context.Background(), "ws"+strings.TrimPrefix(server.URL, "http"), "", "", nil, nil, c, map[string]*call.Handler{}, map[string]string{}); err == nil {
		t.Fatal("lost event socket returned no error")
	}
	c.mu.Lock()
	uncertain, available := pending.eventsUncertain, c.eventsAvailable
	c.mu.Unlock()
	if !uncertain || available {
		t.Fatalf("socket loss was not retained: uncertain=%v available=%v", uncertain, available)
	}
}
