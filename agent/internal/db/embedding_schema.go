package db

import (
	"context"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Preserve legacy documents and vectors, removing the old model-specific size.
// Unlabelled vectors are re-indexed before use with a configured provider.
const embeddingSchema = `
SELECT pg_advisory_xact_lock(hashtext('prismvox:embedding-schema'));
DO $$ BEGIN
    IF to_regclass('rag_chunks') IS NOT NULL THEN
        EXECUTE format('DROP INDEX IF EXISTS %I.rag_chunks_embedding_idx', current_schema());
        IF EXISTS (SELECT 1 FROM pg_attribute WHERE attrelid='rag_chunks'::regclass
                   AND attname='embedding' AND atttypmod <> -1) THEN
            ALTER TABLE rag_chunks ALTER COLUMN embedding TYPE vector USING embedding::vector;
        END IF;
        ALTER TABLE rag_chunks ADD COLUMN IF NOT EXISTS embedding_key TEXT NOT NULL DEFAULT '';
        CREATE INDEX IF NOT EXISTS rag_chunks_embedding_key_idx ON rag_chunks(embedding_key);
    END IF;
END $$;
`

func migrateEmbeddingSchema(ctx context.Context, pool *pgxpool.Pool) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, embeddingSchema); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
