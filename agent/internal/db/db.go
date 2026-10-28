// Package db provides pgx connection pool and typed queries for Prism Vox.
package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Connect creates a pgx connection pool from the DATABASE_URL environment variable.
//
// It retries the initial connectivity check for a bounded window — PostgreSQL may
// still be starting when the agent boots. If the DB is *still* unreachable after the
// retries, it returns the pool anyway together with a non-nil error: pgxpool reconnects
// lazily on subsequent queries, so the agent self-heals once the DB comes back instead
// of running DB-blind (no directory / no RAG / no contact resolution) for the rest of
// its life. Callers must treat a nil pool — not a non-nil error — as "no database".
func Connect(ctx context.Context) (*pgxpool.Pool, error) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return nil, nil
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, err // DSN parse / config error — unrecoverable
	}

	const attempts = 15
	var pingErr error
	for i := 1; i <= attempts; i++ {
		pingCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		pingErr = pool.Ping(pingCtx)
		cancel()
		if pingErr == nil {
			if err := migrateEmbeddingSchema(ctx, pool); err != nil {
				return pool, fmt.Errorf("embedding schema migration: %w", err)
			}
			if _, err := pool.Exec(ctx, ownershipMigration); err != nil {
				return pool, fmt.Errorf("archive ownership migration: %w", err)
			}
			return pool, nil
		}
		log.Printf("db: PostgreSQL not ready yet (attempt %d/%d): %v", i, attempts, pingErr)
		select {
		case <-ctx.Done():
			pool.Close()
			return nil, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	// Keep the (non-nil) pool so it can reconnect on demand once the DB is back.
	return pool, fmt.Errorf("postgres unreachable after %d attempts: %w", attempts, pingErr)
}

// InsertCallLog inserts a new call_logs row and returns the generated ID.
// direction is "inbound" or "outbound".
func InsertCallLog(ctx context.Context, pool *pgxpool.Pool, channelID, callerNumber, direction string) (int64, error) {
	var id int64
	var callerArg interface{}
	if callerNumber != "" {
		callerArg = callerNumber
	}
	if direction == "" {
		direction = "inbound"
	}
	err := pool.QueryRow(ctx,
		"INSERT INTO call_logs (channel_id, caller_number, direction) VALUES ($1, $2, $3) RETURNING id",
		channelID, callerArg, direction,
	).Scan(&id)
	return id, err
}

// CallTask is one outbound-call job claimed from the call_tasks queue.
type CallTask struct {
	ID            int64
	PhoneNumber   string
	ContactName   string
	Mission       string
	Attempts      int
	MaxAttempts   int
	Audience      string
	PrismInstance string
	OwnerID       *int64
}

// ClaimNextCallTask atomically claims the next due pending task: it flips the
// status to 'calling' and increments attempts. Returns nil when the queue is
// empty. FOR UPDATE SKIP LOCKED makes this safe for concurrent agents.
func ClaimNextCallTask(ctx context.Context, pool *pgxpool.Pool) (*CallTask, error) {
	var t CallTask
	var contactName *string
	err := pool.QueryRow(ctx, `
		UPDATE call_tasks SET status='calling', attempts=attempts+1, updated_at=NOW()
		WHERE id = (
			SELECT id FROM call_tasks
			WHERE status='pending' AND scheduled_at <= NOW()
			ORDER BY scheduled_at
			LIMIT 1
			FOR UPDATE SKIP LOCKED
		)
		RETURNING id, phone_number, contact_name, mission, attempts, max_attempts, audience, prism_instance, owner_id`,
	).Scan(&t.ID, &t.PhoneNumber, &contactName, &t.Mission, &t.Attempts, &t.MaxAttempts, &t.Audience, &t.PrismInstance, &t.OwnerID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	if contactName != nil {
		t.ContactName = *contactName
	}
	return &t, nil
}

// RecoverInterruptedTasks reconciles abandoned claims with Asterisk before
// another attempt is allowed. Never redial automatically after an unknown outcome.
// A live channel or a task owned by this process is left alone.
func RecoverInterruptedTasks(ctx context.Context, pool *pgxpool.Pool, live map[string]struct{}, owned map[int64]bool) (int, error) {
	rows, err := pool.Query(ctx, "SELECT id FROM call_tasks WHERE status='calling' AND updated_at < NOW() - INTERVAL '90 seconds'")
	if err != nil {
		return 0, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, err
	}
	n := 0
	for _, id := range ids {
		if owned[id] || hasLiveTaskChannel(id, live) {
			continue
		}
		result, e := pool.Exec(ctx, `UPDATE call_tasks SET status='failed', updated_at=NOW(),
 result_summary=COALESCE(result_summary,'Agent interrupted; no live call remains. Review before retrying.')
 WHERE id=$1 AND status='calling' AND updated_at < NOW() - INTERVAL '90 seconds'`, id)
		if e != nil {
			return n, e
		}
		n += int(result.RowsAffected())
	}
	return n, nil
}

func hasLiveTaskChannel(id int64, live map[string]struct{}) bool {
	prefix := fmt.Sprintf("outbound-%d-", id)
	for cid := range live {
		if strings.HasPrefix(cid, prefix) {
			return true
		}
	}
	return false
}

// FinishCallTask records a terminal outcome for a claimed task.
func FinishCallTask(ctx context.Context, pool *pgxpool.Pool, id int64, status string, summary *string, callLogID *int64) error {
	_, err := pool.Exec(ctx, `
		UPDATE call_tasks
		SET status=$2, result_summary=COALESCE($3, result_summary),
		    call_log_id=COALESCE($4, call_log_id), updated_at=NOW()
		WHERE id=$1`,
		id, status, summary, callLogID,
	)
	return err
}

// RetryCallTask puts a claimed task back in the queue after retryDelay, or
// marks it with finalStatus when max_attempts is exhausted.
func RetryCallTask(ctx context.Context, pool *pgxpool.Pool, id int64, finalStatus string, retryDelay time.Duration) error {
	_, err := pool.Exec(ctx, `
		UPDATE call_tasks
		SET status       = CASE WHEN attempts >= max_attempts THEN $2 ELSE 'pending' END,
		    scheduled_at = CASE WHEN attempts >= max_attempts THEN scheduled_at ELSE NOW() + $3::interval END,
		    updated_at   = NOW()
		WHERE id=$1`,
		id, finalStatus, retryDelay.String(),
	)
	return err
}

// FinalizeCallLog updates a call_logs row with end-of-call data.
func FinalizeCallLog(
	ctx context.Context,
	pool *pgxpool.Pool,
	id int64,
	endReason string,
	toolsUsed []map[string]interface{},
	ragUsed bool,
	summary *string,
	message *string,
	turns []map[string]string,
) error {
	toolsJSON, err := json.Marshal(toolsUsed)
	if err != nil {
		toolsJSON = []byte("[]")
	}
	turnsJSON, err := json.Marshal(turns)
	if err != nil {
		turnsJSON = []byte("[]")
	}
	_, err = pool.Exec(ctx,
		`UPDATE call_logs
		 SET ended_at=NOW(), end_reason=$1, tools_used=$2::jsonb,
		     rag_used=$3, summary=$4, message=$5, turns=$6::jsonb
		 WHERE id=$7`,
		endReason, string(toolsJSON), ragUsed, summary, message, string(turnsJSON), id,
	)
	return err
}

const ownershipMigration = `
 ALTER TABLE call_logs ADD COLUMN IF NOT EXISTS audience TEXT NOT NULL DEFAULT 'unclassified';
 ALTER TABLE call_logs ADD COLUMN IF NOT EXISTS prism_instance TEXT NOT NULL DEFAULT '';
 ALTER TABLE call_logs ADD COLUMN IF NOT EXISTS owner_id BIGINT;
 ALTER TABLE call_tasks ADD COLUMN IF NOT EXISTS audience TEXT NOT NULL DEFAULT 'unclassified';
 ALTER TABLE call_tasks ADD COLUMN IF NOT EXISTS prism_instance TEXT NOT NULL DEFAULT '';
 ALTER TABLE call_tasks ADD COLUMN IF NOT EXISTS owner_id BIGINT;`

func SetCallAudience(ctx context.Context, pool *pgxpool.Pool, id int64, audience, instance string, owner *int64) error {
	_, err := pool.Exec(ctx, `UPDATE call_logs SET audience=$2,prism_instance=$3,owner_id=$4 WHERE id=$1`, id, audience, instance, owner)
	return err
}
