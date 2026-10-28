package db

import (
	"context"
	"fmt"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"os"
	"strings"
	"testing"
	"time"
)

func TestEmbeddingMigrationRetainsLegacyDocuments(t *testing.T) {
	dsn := os.Getenv("VOX_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("requires disposable PostgreSQL")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	if _, err = admin.Exec(ctx, "CREATE EXTENSION IF NOT EXISTS vector WITH SCHEMA public"); err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("embedding_%d", time.Now().UnixNano())
	ident := pgx.Identifier{schema}.Sanitize()
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+ident); err != nil {
		t.Fatal(err)
	}
	defer admin.Exec(ctx, "DROP SCHEMA "+ident+" CASCADE")
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = ident + ",public"
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if _, err = pool.Exec(ctx, `CREATE TABLE rag_chunks(id serial PRIMARY KEY,content text,embedding vector(384));CREATE INDEX ON rag_chunks USING ivfflat(embedding vector_cosine_ops) WITH(lists=1)`); err != nil {
		t.Fatal(err)
	}
	legacy := "[1," + strings.Repeat("0,", 382) + "0]"
	if _, err = pool.Exec(ctx, "INSERT INTO rag_chunks(content,embedding) VALUES('legacy document',$1::vector)", legacy); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err = migrateEmbeddingSchema(ctx, pool); err != nil {
			t.Fatal(err)
		}
	}
	var text, key string
	var dimensions int
	if err = pool.QueryRow(ctx, "SELECT content,embedding_key,vector_dims(embedding) FROM rag_chunks").Scan(&text, &key, &dimensions); err != nil || text != "legacy document" || key != "" || dimensions != 384 {
		t.Fatal("legacy data lost", err)
	}
	large := "[1," + strings.Repeat("0,", 4094) + "0]"
	if _, err = pool.Exec(ctx, "INSERT INTO rag_chunks(content,embedding,embedding_key) VALUES('new document',$1::vector,'provider-4096')", large); err != nil {
		t.Fatal("provider dimensions still constrained", err)
	}
}
