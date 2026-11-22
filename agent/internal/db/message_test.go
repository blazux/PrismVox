package db

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func messageDatabase(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("VOX_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("requires disposable PostgreSQL")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	schema := pgx.Identifier{fmt.Sprintf("db_message_%d", time.Now().UnixNano())}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = 2
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
		admin.Close()
	})
	if _, err := pool.Exec(ctx, `CREATE TABLE call_logs(id bigint PRIMARY KEY,message text CHECK(message NOT LIKE '%reject-fixture%'));
INSERT INTO call_logs VALUES(1,'previous message')`); err != nil {
		t.Fatal(err)
	}
	return pool
}

func TestSaveCallMessageRejectsUnavailableLog(t *testing.T) {
	if err := SaveCallMessage(context.Background(), nil, 1, "Synthetic"); !errors.Is(err, ErrCallLogUnavailable) {
		t.Fatal(err)
	}
}

func TestSaveCallMessageCommitsBeforeSuccess(t *testing.T) {
	pool := messageDatabase(t)
	ctx := context.Background()
	observer, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer observer.Release()
	if err := SaveCallMessage(ctx, pool, 1, "Synthetic recorded message"); err != nil {
		t.Fatal(err)
	}
	var message string
	if err := observer.QueryRow(ctx, "SELECT message FROM call_logs WHERE id=1").Scan(&message); err != nil || message != "Synthetic recorded message" {
		t.Fatalf("write not visible from another connection: %q %v", message, err)
	}
	for _, id := range []int64{0, 99} {
		if err := SaveCallMessage(ctx, pool, id, "Must not replace existing message"); err == nil {
			t.Fatalf("missing log %d reported success", id)
		}
	}
	if err := SaveCallMessage(ctx, pool, 1, "reject-fixture"); err == nil {
		t.Fatal("SQL failure reported success")
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := SaveCallMessage(cancelled, pool, 1, "Cancelled replacement"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := observer.QueryRow(ctx, "SELECT message FROM call_logs WHERE id=1").Scan(&message); err != nil || message != "Synthetic recorded message" {
		t.Fatalf("failed replacement damaged prior capture: %q %v", message, err)
	}
}
