package db

import (
	"context"
	"github.com/jackc/pgx/v5/pgxpool"
	"os"
	"testing"
)

func TestLiveTaskChannelMatch(t *testing.T) {
	live := map[string]struct{}{"outbound-10-abc": {}}
	if hasLiveTaskChannel(1, live) || !hasLiveTaskChannel(10, live) {
		t.Fatal("incorrect task/channel match")
	}
}

func TestRecoveryWithDisposableDatabase(t *testing.T) {
	dsn := os.Getenv("VOX_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set VOX_TEST_DATABASE_URL to a disposable database")
	}
	ctx := context.Background()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	c, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Exec(ctx, `CREATE TEMP TABLE call_tasks(id bigint PRIMARY KEY,status text,updated_at timestamptz,result_summary text);
 INSERT INTO call_tasks VALUES (1,'calling',NOW()-INTERVAL '5 minutes',NULL),(2,'calling',NOW()-INTERVAL '5 minutes',NULL),(3,'calling',NOW()-INTERVAL '5 minutes',NULL),(4,'calling',NOW(),NULL),(5,'pending',NOW()-INTERVAL '5 minutes',NULL)`)
	if err != nil {
		t.Fatal(err)
	}
	// Pin a single connection so the function sees only the temporary fixture table.
	c.Release()
	// A dedicated pool of size one reuses that fixture connection.
	// The integration harness configures pool_max_conns=1 for this test.
	n, err := RecoverInterruptedTasks(ctx, pool, map[string]struct{}{"outbound-2-abc": {}}, map[int64]bool{3: true})
	if err != nil || n != 1 {
		t.Fatalf("recovery: %d %v", n, err)
	}
	var status string
	pool.QueryRow(ctx, "SELECT status FROM call_tasks WHERE id=1").Scan(&status)
	if status != "failed" {
		t.Fatal(status)
	}
	n, err = RecoverInterruptedTasks(ctx, pool, nil, map[int64]bool{2: true, 3: true})
	if err != nil || n != 0 {
		t.Fatalf("idempotence: %d %v", n, err)
	}
}
