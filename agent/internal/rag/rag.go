// Package rag retrieves knowledge base content from the database for RAG injection.
//
// v2: pgvector semantic search. The caller's question is embedded and the top-k
// most similar chunks are returned. Falls back to dumping all chunks when no query
// vector is available (e.g. the greeting) or when no chunk has an embedding yet
// (knowledge base not embedded — run the UI re-embed / re-upload).
package rag

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

const maxContextChars = 12_000

// GetContext returns RAG context for a turn. queryVec is the embedding of the
// caller's question; pass nil to force the dump-all fallback. topK bounds the
// number of chunks returned in semantic mode.
func GetContext(ctx context.Context, pool *pgxpool.Pool, queryVec []float32, embeddingKey string, topK int) (string, error) {
	if len(queryVec) > 0 {
		if out, err := semanticSearch(ctx, pool, queryVec, embeddingKey, topK); err != nil {
			log.Printf("rag: semantic search error: %v — falling back to dump-all", err)
		} else if out != "" {
			return out, nil
		}
		// out == "" → no embedded chunks yet; fall through to dump-all.
	}
	return dumpAll(ctx, pool)
}

// semanticSearch returns the top-k chunks by cosine distance to queryVec. Returns
// "" (no error) when no chunk has an embedding yet, so the caller can fall back.
func semanticSearch(ctx context.Context, pool *pgxpool.Pool, queryVec []float32, embeddingKey string, topK int) (string, error) {
	if topK <= 0 {
		topK = 5
	}
	rows, err := pool.Query(ctx, `
		SELECT rc.name, rk.content
		FROM   rag_chunks rk
		JOIN   rag_collections rc ON rk.collection_id = rc.id
		WHERE  rc.enabled = TRUE AND rk.embedding IS NOT NULL
		  AND rk.embedding_key = $3 AND vector_dims(rk.embedding) = $4
		ORDER  BY rk.embedding <=> $1::vector
		LIMIT  $2
	`, vectorString(queryVec), topK, embeddingKey, len(queryVec))
	if err != nil {
		return "", err
	}
	defer rows.Close()
	return collect(rows)
}

// dumpAll concatenates every chunk of the enabled collections (the v1 behaviour).
func dumpAll(ctx context.Context, pool *pgxpool.Pool) (string, error) {
	rows, err := pool.Query(ctx, `
		SELECT rc.name, rk.content
		FROM   rag_chunks rk
		JOIN   rag_collections rc ON rk.collection_id = rc.id
		WHERE  rc.enabled = TRUE
		ORDER  BY rc.name, rk.chunk_index
	`)
	if err != nil {
		log.Printf("rag: query error: %v", err)
		return "", fmt.Errorf("rag: query error: %w", err)
	}
	defer rows.Close()
	return collect(rows)
}

// rowScanner is the subset of pgx.Rows used by collect.
type rowScanner interface {
	Next() bool
	Scan(...any) error
}

// collect groups (name, content) rows by collection name, bounded by maxContextChars.
func collect(rows rowScanner) (string, error) {
	sections := make(map[string][]string)
	var order []string
	total := 0

	for rows.Next() {
		var name, content string
		if err := rows.Scan(&name, &content); err != nil {
			continue
		}
		if total+len(content) > maxContextChars {
			break
		}
		if _, exists := sections[name]; !exists {
			order = append(order, name)
		}
		sections[name] = append(sections[name], content)
		total += len(content)
	}

	if len(sections) == 0 {
		return "", nil
	}

	parts := make([]string, 0, len(order))
	for _, name := range order {
		parts = append(parts, "### "+name+"\n"+strings.Join(sections[name], "\n---\n"))
	}
	return strings.Join(parts, "\n\n"), nil
}

// vectorString formats a float slice as a pgvector literal: [0.1,0.2,…].
func vectorString(vec []float32) string {
	var b strings.Builder
	b.WriteByte('[')
	for i, v := range vec {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatFloat(float64(v), 'f', -1, 32))
	}
	b.WriteByte(']')
	return b.String()
}
