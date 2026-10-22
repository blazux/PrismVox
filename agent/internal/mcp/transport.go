package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const maxResponse = 16 << 20

type rpcReply struct {
	ID     int64                  `json:"id"`
	Result map[string]interface{} `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

type rpcSession struct {
	endpoint, token, sessionID, version string
	http                                *http.Client
	seq                                 atomic.Int64
	callMu                              sync.Mutex
	mu                                  sync.Mutex
	waiting                             map[int64]chan rpcReply
	stream                              io.ReadCloser
	cancel                              context.CancelFunc
	done                                chan struct{}
	closed                              sync.Once
}

func sameOrigin(a, b *url.URL) bool { return a.Scheme == b.Scheme && a.Host == b.Host }

func openSession(ctx context.Context, address, token, kind string) (*rpcSession, error) {
	u, err := url.Parse(address)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil {
		return nil, errors.New("MCP requires an HTTP(S) URL without embedded credentials")
	}
	ctx, cancel := context.WithCancel(ctx)
	c := &rpcSession{endpoint: address, token: token, cancel: cancel, done: make(chan struct{}), waiting: make(map[int64]chan rpcReply)}
	c.http = &http.Client{Transport: &http.Transport{Proxy: http.ProxyFromEnvironment, ResponseHeaderTimeout: 10 * time.Second}, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 || !sameOrigin(u, req.URL) {
			return errors.New("MCP redirect outside configured origin refused")
		}
		return nil
	}}
	if kind == "sse" {
		if err = c.openSSE(ctx); err != nil {
			c.Close()
			return nil, err
		}
	}
	_, err = c.call(ctx, "initialize", map[string]interface{}{"protocolVersion": "2025-03-26", "capabilities": map[string]interface{}{}, "clientInfo": map[string]string{"name": "PrismVox", "version": "1"}})
	if err != nil && kind != "sse" && (strings.Contains(err.Error(), "HTTP 404") || strings.Contains(err.Error(), "HTTP 405")) {
		if err = c.openSSE(ctx); err == nil {
			_, err = c.call(ctx, "initialize", map[string]interface{}{"protocolVersion": "2025-03-26", "capabilities": map[string]interface{}{}, "clientInfo": map[string]string{"name": "PrismVox", "version": "1"}})
		}
	}
	if err != nil {
		c.Close()
		return nil, err
	}
	if err = c.notify(ctx, "notifications/initialized"); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

func (c *rpcSession) request(ctx context.Context, method, address string, data []byte) (*http.Response, error) {
	r, err := http.NewRequestWithContext(ctx, method, address, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	r.Header.Set("Accept", "application/json, text/event-stream")
	if method == "POST" {
		r.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		r.Header.Set("Authorization", "Bearer "+c.token)
	}
	if c.sessionID != "" {
		r.Header.Set("Mcp-Session-Id", c.sessionID)
	}
	if c.version != "" {
		r.Header.Set("MCP-Protocol-Version", c.version)
	}
	return c.http.Do(r)
}

func checkReply(r rpcReply, id int64) (map[string]interface{}, error) {
	if r.ID != id {
		return nil, errors.New("MCP returned an unexpected response id")
	}
	if r.Error != nil {
		return nil, fmt.Errorf("MCP RPC error %d", r.Error.Code)
	}
	if r.Result == nil {
		return nil, errors.New("MCP response has no result")
	}
	return r.Result, nil
}

func (c *rpcSession) call(ctx context.Context, method string, params interface{}) (map[string]interface{}, error) {
	c.callMu.Lock()
	defer c.callMu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	id := c.seq.Add(1)
	data, err := json.Marshal(map[string]interface{}{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	if err != nil {
		return nil, err
	}
	response := make(chan rpcReply, 1)
	c.mu.Lock()
	legacy := c.stream != nil
	endpoint := c.endpoint
	if legacy {
		c.waiting[id] = response
	}
	c.mu.Unlock()
	defer func() { c.mu.Lock(); delete(c.waiting, id); c.mu.Unlock() }()
	r, err := c.request(ctx, "POST", endpoint, data)
	if err != nil {
		return nil, err
	}
	defer r.Body.Close()
	if r.StatusCode >= 400 {
		return nil, fmt.Errorf("MCP HTTP %d", r.StatusCode)
	}
	if sid := r.Header.Get("Mcp-Session-Id"); sid != "" {
		c.sessionID = sid
	}
	var reply rpcReply
	if legacy && r.StatusCode == http.StatusAccepted {
		select {
		case reply = <-response:
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-c.done:
			return nil, errors.New("MCP SSE connection closed")
		}
	} else if strings.Contains(r.Header.Get("Content-Type"), "text/event-stream") {
		found := false
		err = readEvents(r.Body, func(event, data string) bool {
			var candidate rpcReply
			if json.Unmarshal([]byte(data), &candidate) == nil && candidate.ID == id {
				reply = candidate
				found = true
				return false
			}
			return true
		})
		if err != nil {
			return nil, err
		}
		if !found {
			return nil, errors.New("MCP SSE response ended without a result")
		}
	} else {
		body, e := io.ReadAll(io.LimitReader(r.Body, maxResponse+1))
		if e != nil {
			return nil, e
		}
		if len(body) > maxResponse {
			return nil, errors.New("MCP response too large")
		}
		if err = json.Unmarshal(body, &reply); err != nil {
			return nil, errors.New("invalid MCP JSON response")
		}
	}
	result, err := checkReply(reply, id)
	if err == nil && method == "initialize" {
		c.version, _ = result["protocolVersion"].(string)
	}
	return result, err
}

func (c *rpcSession) notify(ctx context.Context, method string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	data, _ := json.Marshal(map[string]interface{}{"jsonrpc": "2.0", "method": method})
	r, err := c.request(ctx, "POST", c.endpoint, data)
	if err != nil {
		return err
	}
	defer r.Body.Close()
	if r.StatusCode >= 400 {
		return fmt.Errorf("MCP notification HTTP %d", r.StatusCode)
	}
	return nil
}

// readEvents handles multiline SSE data, heartbeats and bounded event sizes.
func readEvents(r io.Reader, visit func(string, string) bool) error {
	s := bufio.NewScanner(r)
	s.Buffer(make([]byte, 4096), maxResponse)
	var event string
	var data []string
	size := 0
	flush := func() bool {
		if len(data) == 0 {
			return true
		}
		ok := visit(event, strings.Join(data, "\n"))
		event = ""
		data = nil
		size = 0
		return ok
	}
	for s.Scan() {
		line := s.Text()
		if line == "" {
			if !flush() {
				return nil
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		field, value, _ := strings.Cut(line, ":")
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "event":
			event = value
		case "data":
			size += len(value)
			if size > maxResponse {
				return errors.New("MCP event too large")
			}
			data = append(data, value)
		}
	}
	if err := s.Err(); err != nil {
		return err
	}
	flush()
	return nil
}

func (c *rpcSession) openSSE(ctx context.Context) error {
	original, _ := url.Parse(c.endpoint)
	r, err := c.request(ctx, "GET", c.endpoint, nil)
	if err != nil {
		return err
	}
	if r.StatusCode != 200 {
		r.Body.Close()
		return fmt.Errorf("MCP SSE HTTP %d", r.StatusCode)
	}
	c.mu.Lock()
	c.stream = r.Body
	c.mu.Unlock()
	ready := make(chan error, 1)
	endpointSet := false
	go func() {
		defer c.Close()
		_ = readEvents(r.Body, func(event, data string) bool {
			if event == "endpoint" {
				if endpointSet {
					return true
				}
				endpointSet = true
				u, e := url.Parse(data)
				if e != nil {
					select {
					case ready <- e:
					default:
					}
					return false
				}
				u = original.ResolveReference(u)
				if !sameOrigin(original, u) || u.User != nil {
					select {
					case ready <- errors.New("MCP SSE endpoint outside configured origin refused"):
					default:
					}
					return false
				}
				c.mu.Lock()
				c.endpoint = u.String()
				c.mu.Unlock()
				select {
				case ready <- nil:
				default:
				}
				return true
			}
			var reply rpcReply
			if json.Unmarshal([]byte(data), &reply) == nil {
				c.mu.Lock()
				ch := c.waiting[reply.ID]
				c.mu.Unlock()
				if ch != nil {
					select {
					case ch <- reply:
					default:
					}
				}
			}
			return true
		})
	}()
	select {
	case err = <-ready:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-c.done:
		select {
		case err = <-ready:
			return err
		default:
			return errors.New("MCP SSE closed before endpoint")
		}
	case <-time.After(10 * time.Second):
		return errors.New("MCP SSE endpoint timed out")
	}
}

func (c *rpcSession) Close() {
	c.closed.Do(func() {
		c.cancel()
		c.mu.Lock()
		if c.stream != nil {
			c.stream.Close()
		}
		c.mu.Unlock()
		close(c.done)
		c.http.CloseIdleConnections()
	})
}
