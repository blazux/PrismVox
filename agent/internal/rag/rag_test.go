package rag

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

func TestSemanticSearchExcludesOtherModelsAndDimensions(t *testing.T) {
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
	ident := pgx.Identifier{fmt.Sprintf("rag_%d", time.Now().UnixNano())}.Sanitize()
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
	_, err = pool.Exec(ctx, `CREATE TABLE rag_collections(id int PRIMARY KEY,name text,enabled boolean);INSERT INTO rag_collections VALUES(1,'fixture',true);
 CREATE TABLE rag_chunks(collection_id int,content text,chunk_index int,embedding vector,embedding_key text);
 INSERT INTO rag_chunks VALUES(1,'current content',0,'[1,0]','current'),(1,'other model',1,'[1,0,0]','other'),(1,'wrong dimensions',2,'[1,0,0]','current')`)
	if err != nil {
		t.Fatal(err)
	}
	result, err := GetContext(ctx, pool, []float32{1, 0}, "current", 5)
	if err != nil || !strings.Contains(result, "current content") || strings.Contains(result, "other model") || strings.Contains(result, "wrong dimensions") {
		t.Fatalf("incompatible vectors reached search: %q %v", result, err)
	}
}
