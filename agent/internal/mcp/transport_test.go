package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestStreamableSessionCatalogueAndCall(t *testing.T) {
	for _, sse := range []bool{false, true} {
		t.Run(fmt.Sprint(sse), func(t *testing.T) {
			var initialized atomic.Bool
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/custom-endpoint" || r.Header.Get("Authorization") != "Bearer fixture" || r.Header.Get("Accept") == "" {
					t.Error("endpoint/headers changed")
					w.WriteHeader(400)
					return
				}
				var q struct {
					ID     int64                  `json:"id"`
					Method string                 `json:"method"`
					Params map[string]interface{} `json:"params"`
				}
				json.NewDecoder(r.Body).Decode(&q)
				var result interface{}
				switch q.Method {
				case "initialize":
					w.Header().Set("Mcp-Session-Id", "fixture-session")
					result = map[string]interface{}{"protocolVersion": "2025-03-26", "capabilities": map[string]interface{}{}, "serverInfo": map[string]string{"name": "fixture", "version": "1"}}
				case "notifications/initialized":
					initialized.Store(true)
					w.WriteHeader(202)
					return
				case "tools/list":
					if !initialized.Load() || r.Header.Get("Mcp-Session-Id") != "fixture-session" || r.Header.Get("MCP-Protocol-Version") == "" {
						t.Error("uninitialized session")
						w.WriteHeader(400)
						return
					}
					tool := map[string]interface{}{"name": "first", "inputSchema": map[string]string{"type": "object"}}
					result = map[string]interface{}{"tools": []interface{}{tool}, "nextCursor": "next"}
					if q.Params["cursor"] == "next" {
						tool["name"] = "second"
						result = map[string]interface{}{"tools": []interface{}{tool}}
					}
				case "tools/call":
					result = map[string]interface{}{"content": []interface{}{map[string]string{"type": "text", "text": "one"}, map[string]string{"type": "text", "text": "two"}}}
				default:
					t.Error("unexpected method", q.Method)
					w.WriteHeader(400)
					return
				}
				body, _ := json.Marshal(map[string]interface{}{"jsonrpc": "2.0", "id": q.ID, "result": result})
				if sse {
					w.Header().Set("Content-Type", "text/event-stream")
					fmt.Fprintf(w, ": heartbeat\n\nevent: message\ndata: %s\n\n", body)
				} else {
					w.Header().Set("Content-Type", "application/json")
					w.Write(body)
				}
			}))
			defer srv.Close()
			c, err := openSession(context.Background(), srv.URL+"/custom-endpoint", "fixture", "http")
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			tools, err := listSessionTools(context.Background(), c)
			if err != nil || len(tools) != 2 {
				t.Fatalf("catalogue: %v %v", tools, err)
			}
			m := NewManager()
			m.tools["first"] = toolEntry{Tool: tools[0], client: c}
			out, err := m.CallTool(context.Background(), "first", nil)
			if err != nil || out != "one\ntwo" {
				t.Fatalf("result: %q %v", out, err)
			}
			m.CloseAll()
		})
	}
}

func TestLegacyPersistentSSE(t *testing.T) {
	messages := make(chan []byte, 8)
	var initialized atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/sse" {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "event: endpoint\ndata: /messages\n\n")
			w.(http.Flusher).Flush()
			for {
				select {
				case b := <-messages:
					fmt.Fprintf(w, "event: message\ndata: %s\n\n", b)
					w.(http.Flusher).Flush()
				case <-r.Context().Done():
					return
				}
			}
		}
		if r.URL.Path != "/messages" {
			w.WriteHeader(404)
			return
		}
		var q struct {
			ID     int64  `json:"id"`
			Method string `json:"method"`
		}
		json.NewDecoder(r.Body).Decode(&q)
		if q.Method == "notifications/initialized" {
			initialized.Store(true)
			w.WriteHeader(202)
			return
		}
		var result interface{} = map[string]interface{}{"protocolVersion": "2024-11-05"}
		if q.Method == "tools/list" {
			if !initialized.Load() {
				t.Error("missing initialized notification")
			}
			result = map[string]interface{}{"tools": []interface{}{map[string]interface{}{"name": "legacy", "inputSchema": map[string]string{"type": "object"}}}}
		}
		b, _ := json.Marshal(map[string]interface{}{"jsonrpc": "2.0", "id": q.ID, "result": result})
		messages <- b
		w.WriteHeader(202)
	}))
	defer srv.Close()
	c, err := openSession(context.Background(), srv.URL+"/sse", "", "sse")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	tools, err := listSessionTools(context.Background(), c)
	if err != nil || len(tools) != 1 || tools[0].Name != "legacy" {
		t.Fatalf("legacy: %v %v", tools, err)
	}
}

func TestLegacyEndpointCannotLeakCredentials(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: endpoint\ndata: https://other.invalid/messages\n\n")
		w.(http.Flusher).Flush()
	}))
	defer srv.Close()
	if _, err := openSession(context.Background(), srv.URL, "fixture", "sse"); err == nil || !strings.Contains(err.Error(), "origin") {
		t.Fatalf("foreign endpoint accepted: %v", err)
	}
}

func TestMultilineSSEAndBounds(t *testing.T) {
	var got string
	err := readEvents(strings.NewReader(": heartbeat\nevent: message\ndata: {\ndata: \"id\": 1}\n\n"), func(event, data string) bool { got = data; return false })
	if err != nil || got != "{\n\"id\": 1}" {
		t.Fatalf("multiline: %q %v", got, err)
	}
	if err = readEvents(strings.NewReader("data: "+strings.Repeat("x", maxResponse+1)), func(string, string) bool { return true }); err == nil {
		t.Fatal("oversized SSE accepted")
	}
}
