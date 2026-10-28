// Package embeddings calls an OpenAI-compatible embedding endpoint (Ollama,
// vLLM or another provider). Settings are captured once per phone call.
package embeddings

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"phoneagent/internal/secrets"
)

type Client struct {
	baseURL, model, apiKey string
	dimensions             int
	configErr              error
	http                   *http.Client
}

func New() *Client {
	c := &Client{http: &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}}
	c.Configure(nil)
	return c
}

func (c *Client) Configure(raw map[string]string) {
	value := func(key, env string) string {
		if v, ok := raw[key]; ok {
			return strings.TrimSpace(v)
		}
		return strings.TrimSpace(os.Getenv(env))
	}
	c.baseURL = strings.TrimRight(value("embedding_url", "EMBEDDING_BASE_URL"), "/")
	c.model = value("embedding_model", "EMBEDDING_MODEL")
	c.dimensions, _ = strconv.Atoi(strings.TrimSpace(raw["embedding_dimensions"]))
	c.apiKey, c.configErr = secrets.Decrypt(value("embedding_api_key", "EMBEDDING_API_KEY"), "embedding_api_key")
	if c.configErr != nil {
		return
	}
	if c.baseURL == "" && c.model == "" {
		return
	}
	u, err := url.Parse(c.baseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || c.model == "" {
		c.configErr = fmt.Errorf("configure the embedding API URL and model in Knowledge Base")
	}
}

func (c *Client) Configured() bool { return c.configErr == nil && c.baseURL != "" && c.model != "" }

// Key identifies compatible vectors, including the dimension actually returned.
func (c *Client) Key(dimensions int) string {
	sum := sha256.Sum256([]byte(c.baseURL + "\n" + c.model + "\n" + strconv.Itoa(dimensions)))
	return hex.EncodeToString(sum[:])
}

func (c *Client) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	if c.configErr != nil {
		return nil, c.configErr
	}
	if !c.Configured() {
		return nil, fmt.Errorf("embedding provider is not configured")
	}
	body, _ := json.Marshal(map[string]interface{}{"model": c.model, "input": texts, "encoding_format": "float"})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("invalid embedding endpoint")
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embedding endpoint unavailable")
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("embedding endpoint returned HTTP %d", resp.StatusCode)
	}
	var result struct {
		Data []struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	if err = json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&result); err != nil {
		return nil, fmt.Errorf("invalid embedding response")
	}
	if len(result.Data) != len(texts) {
		return nil, fmt.Errorf("embedding response count does not match input")
	}
	vectors := make([][]float32, len(texts))
	dim := c.dimensions
	for _, item := range result.Data {
		if item.Index < 0 || item.Index >= len(vectors) || vectors[item.Index] != nil || len(item.Embedding) == 0 || len(item.Embedding) > 16000 {
			return nil, fmt.Errorf("invalid embedding response index or dimension")
		}
		if dim == 0 {
			dim = len(item.Embedding)
		}
		if len(item.Embedding) != dim {
			return nil, fmt.Errorf("embedding dimension changed; test the provider and re-index documents")
		}
		nonzero := false
		for _, v := range item.Embedding {
			if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) {
				return nil, fmt.Errorf("non-finite embedding")
			}
			nonzero = nonzero || v != 0
		}
		if !nonzero {
			return nil, fmt.Errorf("embedding must not be a zero vector")
		}
		vectors[item.Index] = item.Embedding
	}
	return vectors, nil
}

func (c *Client) EmbedOne(ctx context.Context, text string) ([]float32, error) {
	vectors, err := c.Embed(ctx, []string{text})
	if err != nil {
		return nil, err
	}
	return vectors[0], nil
}
