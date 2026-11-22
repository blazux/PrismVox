package main

import (
	"context"
	"fmt"
	"github.com/jackc/pgx/v5/pgxpool"
	"net/http"
	"net/http/httptest"
	"os"
	"phoneagent/internal/ari"
	"phoneagent/internal/call"
	"phoneagent/internal/db"
	"sync"
	"testing"
	"time"
)

func outboundTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	if os.Getenv("VOX_TEST_DATABASE_URL") == "" {
		t.Skip("requires disposable PostgreSQL")
	}
	ctx := context.Background()
	root, err := pgxpool.New(ctx, os.Getenv("VOX_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("review_%d", time.Now().UnixNano())
	if _, err = root.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	cfg, err := pgxpool.ParseConfig(os.Getenv("VOX_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pool.Close(); _, _ = root.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE"); root.Close() })
	_, err = pool.Exec(ctx, `CREATE TABLE config(key text,value text);
 CREATE TABLE call_tasks(id bigint PRIMARY KEY,status text,attempts int,max_attempts int,scheduled_at timestamptz DEFAULT now(),updated_at timestamptz DEFAULT now(),result_summary text,call_log_id bigint);
 INSERT INTO call_tasks(id,status,attempts,max_attempts) VALUES(1,'calling',1,3);`)
	if err != nil {
		t.Fatal(err)
	}
	return pool
}

func TestOutboundUncertainRequestNeverAutomaticallyRedials(t *testing.T) {
	for _, observed := range []int{404, 503} {
		t.Run(fmt.Sprint(observed), func(t *testing.T) {
			pool := outboundTestPool(t)
			posted := false
			looked := false
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "POST" {
					posted = true
					conn, _, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					conn.Close()
					return
				}
				if r.Method == "GET" {
					looked = true
					w.WriteHeader(observed)
					return
				}
				w.WriteHeader(204)
			}))
			defer server.Close()
			coord := newCoordinator()
			runOutboundCall(context.Background(), ari.New(server.URL, "", ""), pool, coord, "fixture", &db.CallTask{ID: 1, PhoneNumber: "fixture", Attempts: 1, MaxAttempts: 3})
			var status string
			var summary *string
			if err := pool.QueryRow(context.Background(), "SELECT status,result_summary FROM call_tasks WHERE id=1").Scan(&status, &summary); err != nil {
				t.Fatal(err)
			}
			if !posted || !looked || status != "failed" || summary == nil || *summary == "" || len(coord.ownedTasks()) != 0 {
				t.Fatalf("post=%v reconcile=%v status=%s summary=%v", posted, looked, status, summary)
			}
		})
	}
}
func TestOutboundAcceptedTruncatedBodyKeepsItsOwner(t *testing.T) {
	pool := outboundTestPool(t)
	posted := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			w.Header().Set("Content-Length", "100")
			w.WriteHeader(201)
			w.Write([]byte(`{}`))
			close(posted)
			return
		}
		w.WriteHeader(204)
	}))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	coord := newCoordinator()
	done := make(chan struct{})
	go func() {
		defer close(done)
		runOutboundCall(ctx, ari.New(server.URL, "", ""), pool, coord, "fixture", &db.CallTask{ID: 1, PhoneNumber: "fixture", Attempts: 1, MaxAttempts: 3})
	}()
	<-posted
	select {
	case <-done:
		t.Fatal("accepted request abandoned after response body failure")
	case <-time.After(40 * time.Millisecond):
	}
	var status string
	if err := pool.QueryRow(context.Background(), "SELECT status FROM call_tasks WHERE id=1").Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "calling" || !coord.ownedTasks()[1] {
		t.Fatalf("status=%s owned=%v", status, coord.ownedTasks())
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cancel failed")
	}
}
func TestOutboundOwnershipDecisionIsAtomic(t *testing.T) {
	for n := 0; n < 100; n++ {
		c := newCoordinator()
		p := &outboundPending{task: &db.CallTask{ID: 1}, answered: make(chan *call.Handler, 1)}
		c.addPending("id", p)
		h := &call.Handler{}
		var attached bool
		var handed *call.Handler
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); attached = c.attachPending("id", h) }()
		go func() { defer wg.Done(); handed = c.finishPending("id") }()
		wg.Wait()
		if attached != (handed == h) {
			t.Fatalf("attached=%v handed=%p", attached, handed)
		}
	}
}
func TestOutboundClosureMustBeConfirmed(t *testing.T) {
	for _, status := range []int{200, 302, 404, 503} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "DELETE" {
					w.WriteHeader(503)
				} else {
					w.WriteHeader(status)
				}
			}))
			defer server.Close()
			if got := cancelOutboundChannel(ari.New(server.URL, "", ""), "id"); got != (status == 404) {
				t.Fatalf("status=%d confirmed=%v", status, got)
			}
		})
	}
}
