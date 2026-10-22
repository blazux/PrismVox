package llm

import (
	"context"
	"net/http"
	"net/http/httptest"
	neturl "net/url"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestPrismFragmentedThinkingAndGatewayContext(t *testing.T) {
	for split := 1; split < len("</think>"); split++ {
		t.Run(string(rune('0'+split)), func(t *testing.T) {
			request := make(chan map[string]interface{}, 1)
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer fixture" || r.URL.Path != "/ws" || r.URL.Query().Get("session") != "test" || r.URL.Query().Get("channel") != "voice" || r.URL.Query().Get("caller") != "+12025550199" {
					t.Error("voice handshake lost its credentials, caller or session")
				}
				conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
				if err != nil {
					return
				}
				defer conn.Close()
				var msg map[string]interface{}
				if conn.ReadJSON(&msg) != nil {
					return
				}
				request <- msg
				for _, part := range []string{"<thi", "nk>secret" + "</think>"[:split], "</think>"[split:] + "Bonjour."} {
					_ = conn.WriteJSON(map[string]string{"type": "stream", "content": part})
				}
				_ = conn.WriteJSON(map[string]interface{}{"type": "telephony", "tool": "take_message", "args": map[string]string{"recipient": "Jean", "content": "Rappelez-moi"}})
				_ = conn.WriteJSON(map[string]string{"type": "turn_complete"})
			}))
			defer s.Close()
			c := &Client{PrismURL: "ws" + strings.TrimPrefix(s.URL, "http"), PrismToken: "fixture", Caller: "+12025550199", SessionID: "test", GatewayContext: []string{"Transfert échoué"}}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			sent, calls := make(chan string, 10), make(chan []ToolCall, 1)
			if err := c.streamPrism(ctx, "Bonjour", sent, calls); err != nil {
				t.Fatal(err)
			}
			if len(sent) != 1 {
				t.Fatalf("got %d spoken sentences", len(sent))
			}
			if got := <-sent; got != "Bonjour." {
				t.Fatalf("spoken: %q", got)
			}
			if len(calls) != 1 || (<-calls)[0].Name != "take_message" {
				t.Fatal("lost telephony action")
			}
			if c.GatewayContext != nil {
				t.Fatal("completed facts must not replay next turn")
			}
			if (<-request)["gateway_context"] == nil {
				t.Fatal("missing gateway facts")
			}
		})
	}
}

func TestPrismURLAcceptsDocumentedHTTPAddress(t *testing.T) {
	for base, want := range map[string]string{
		"http://prism:8080":           "ws://prism:8080/ws?session=call",
		"https://example.test/prism/": "wss://example.test/prism/ws?session=call",
		"ws://prism:8080":             "ws://prism:8080/ws?session=call",
	} {
		got, err := prismSocketURL(base, neturl.Values{"session": {"call"}})
		if err != nil || got != want {
			t.Fatalf("%s: %q, %v", base, got, err)
		}
	}
	if _, err := prismSocketURL("prism:8080", nil); err == nil {
		t.Fatal("invalid scheme accepted")
	}
}

func TestPrismCancelledTurnDoesNotReleaseQueuedActions(t *testing.T) {
	actionSent := make(chan struct{})
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		var msg map[string]interface{}
		if conn.ReadJSON(&msg) != nil {
			return
		}
		_ = conn.WriteJSON(map[string]string{"type": "telephony", "tool": "end_call"})
		close(actionSent)
		_, _, _ = conn.ReadMessage()
	}))
	defer s.Close()
	c := &Client{PrismURL: s.URL, SessionID: "call"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sent, calls := make(chan string, 10), make(chan []ToolCall, 1)
	done := make(chan error, 1)
	go func() { done <- c.streamPrism(ctx, "Attendez", sent, calls) }()
	select {
	case <-actionSent:
	case <-time.After(time.Second):
		t.Fatal("no action received")
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancellation ignored")
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled stream stuck")
	}
	if len(calls) != 0 {
		t.Fatal("cancelled hangup must not be executed")
	}
}
