package call

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"phoneagent/internal/config"
	"phoneagent/internal/llm"
)

func messageFixture(t *testing.T) *pgxpool.Pool {
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
	schema := pgx.Identifier{fmt.Sprintf("call_message_%d", time.Now().UnixNano())}.Sanitize()
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

func messageHandler(pool *pgxpool.Pool, logID int64) *Handler {
	previous := "previous message"
	return &Handler{pool: pool, logID: logID, message: &previous,
		llmClient: &llm.Client{PrismURL: "http://fixture.invalid", CallerKnown: true, GatewayContext: []string{"Previous confirmed fact"}},
	}
}

func assertMessageFailure(t *testing.T, h *Handler, ctx context.Context, args map[string]interface{}) {
	t.Helper()
	oldMessage := h.message
	oldContext := append([]string(nil), h.llmClient.GatewayContext...)
	before := len(h.toolsUsed)
	reply, err := h.recordMessage(ctx, args)
	if err == nil || strings.Contains(reply, "bien noté") || reply == "" {
		t.Fatalf("failed write sounded successful: %q %v", reply, err)
	}
	if h.message != oldMessage || !reflect.DeepEqual(h.llmClient.GatewayContext, oldContext) {
		t.Fatal("failed write changed the prior message or confirmed gateway facts")
	}
	if len(h.toolsUsed) != before+1 || h.toolsUsed[before]["outcome"] != "failed" {
		t.Fatal("failed write was recorded as successful")
	}
	if strings.Contains(err.Error(), "reject-fixture") {
		t.Fatal("SQL row details leaked through the tool error")
	}
}

func TestRecordMessageWithoutStorageCannotConfirm(t *testing.T) {
	h := messageHandler(nil, 0)
	assertMessageFailure(t, h, context.Background(), map[string]interface{}{"recipient": "Fixture", "content": "Synthetic"})
	assertMessageFailure(t, h, context.Background(), map[string]interface{}{"recipient": "Fixture"})
}

func TestTakeMessageToolWithoutStorageDoesNotAcknowledgeRecording(t *testing.T) {
	h := messageHandler(nil, 0)
	previous := h.message
	reply, _ := h.handleToolCalls(context.Background(), "", []llm.ToolCall{{Name: "take_message", Arguments: map[string]interface{}{"recipient": "Fixture", "content": "Synthetic"}}}, config.FromMap(nil), "", "")
	if reply == "" || strings.Contains(reply, "bien noté") || h.message != previous || len(h.llmClient.GatewayContext) != 1 {
		t.Fatalf("tool acknowledged an unsaved message: %q", reply)
	}
	if len(h.toolsUsed) != 1 || h.toolsUsed[0]["outcome"] != "failed" {
		t.Fatal("tool dispatch recorded an uncommitted message as successful")
	}
}

func TestRecordMessageConfirmsOnlyAfterDatabaseWrite(t *testing.T) {
	pool := messageFixture(t)
	ctx := context.Background()
	h := messageHandler(pool, 1)
	observer, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer observer.Release()
	reply, err := h.recordMessage(ctx, map[string]interface{}{"recipient": " Fixture ", "content": " Synthetic message "})
	if err != nil || reply != "Le message pour Fixture est bien noté." {
		t.Fatalf("saved message not confirmed: %q %v", reply, err)
	}
	var saved string
	if err := observer.QueryRow(ctx, "SELECT message FROM call_logs WHERE id=1").Scan(&saved); err != nil || saved != "Pour Fixture : Synthetic message" || h.message == nil || *h.message != saved {
		t.Fatalf("confirmation preceded durable state: %q %v", saved, err)
	}
	if len(h.toolsUsed) != 1 || h.toolsUsed[0]["outcome"] != "recorded_in_call_log_not_delivered" || len(h.llmClient.GatewayContext) != 2 {
		t.Fatal("saved message lost its confirmed outcome/context")
	}
	for _, logID := range []int64{0, 99} {
		assertMessageFailure(t, messageHandler(pool, logID), ctx, map[string]interface{}{"recipient": "Fixture", "content": "Synthetic"})
	}
	assertMessageFailure(t, h, ctx, map[string]interface{}{"recipient": "Fixture", "content": "reject-fixture"})
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	assertMessageFailure(t, h, cancelled, map[string]interface{}{"recipient": "Fixture", "content": "Cancelled replacement"})
	if err := observer.QueryRow(ctx, "SELECT message FROM call_logs WHERE id=1").Scan(&saved); err != nil || saved != "Pour Fixture : Synthetic message" {
		t.Fatalf("failed write replaced the recorded message: %q %v", saved, err)
	}
}
