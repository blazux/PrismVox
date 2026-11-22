package db

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrCallLogUnavailable = errors.New("call log is unavailable")
	ErrCallLogNotFound    = errors.New("call log was not found")
)

// SaveCallMessage commits the current message before the caller is told it was
// recorded. It does not finalize the call or deliver anything to the recipient.
func SaveCallMessage(ctx context.Context, pool *pgxpool.Pool, id int64, message string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if pool == nil || id <= 0 {
		return ErrCallLogUnavailable
	}
	result, err := pool.Exec(ctx, "UPDATE call_logs SET message=$1 WHERE id=$2", message, id)
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return ErrCallLogNotFound
	}
	return nil
}
