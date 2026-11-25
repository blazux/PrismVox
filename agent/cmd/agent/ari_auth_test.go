package main

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"phoneagent/internal/call"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
)

func TestARIHandshakeAuthenticatesWithoutCredentialsInURL(t *testing.T) {
	const app = "fixture & application"
	const password = "fixture-private-&?#-password"
	seen := make(chan bool, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		valid := ok && user == "fixture-user" && pass == password &&
			r.URL.Query().Get("app") == app && r.URL.Query().Get("subscribeAll") == "false" &&
			r.URL.Query().Get("api_key") == "" && !strings.Contains(r.RequestURI, "password")
		seen <- valid
		if !valid {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err == nil {
			conn.Close()
		}
	}))
	defer server.Close()
	host, port, err := net.SplitHostPort(strings.TrimPrefix(server.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	_ = connectAndServe(context.Background(), ariEventsURL(host, port, app), "fixture-user", password,
		nil, nil, newCoordinator(), map[string]*call.Handler{}, map[string]string{})
	if !<-seen {
		t.Fatal("ARI handshake did not use header-only credentials and an encoded application name")
	}
}
