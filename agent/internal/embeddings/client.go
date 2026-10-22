// Package embeddings is an HTTP client for the sentence-transformers embedding
// service, used to embed the caller's question for pgvector similarity search.
package embeddings

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// Client calls the embeddings service.
type Client struct {
	url  string
	http *http.Client
}

// New creates a Client from the EMBEDDINGS_URL environment variable.
func New() *Client {
	u := os.Getenv("EMBEDDINGS_URL")
	if u == "" {
		u = "http://embeddings:8002"
	}
	return &Client{
		url:  strings.TrimRight(u, "/"),
		http: &http.Client{Timeout: 10 * time.Second},
	}
}

// Embed returns one vector per input text.
func (c *Client) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	body, err := json.Marshal(map[string]interface{}{"texts": texts})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", c.url+"/embed", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("embeddings HTTP %d: %s", resp.StatusCode, data)
	}

	var out struct {
		Embeddings [][]float32 `json:"embeddings"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("embeddings decode: %w", err)
	}
	return out.Embeddings, nil
}

// EmbedOne embeds a single text.
func (c *Client) EmbedOne(ctx context.Context, text string) ([]float32, error) {
	vecs, err := c.Embed(ctx, []string{text})
	if err != nil {
		return nil, err
	}
	if len(vecs) == 0 {
		return nil, fmt.Errorf("embeddings: empty response")
	}
	return vecs[0], nil
}
