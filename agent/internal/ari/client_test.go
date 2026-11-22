package ari

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
)

func TestDeleteRequiresDirect2xxWithoutFollowingRedirects(t *testing.T) {
	for _, status := range []int{200, 204, 302, 304, 307} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var redirected atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/redirected" {
					redirected.Add(1)
					w.WriteHeader(http.StatusOK)
					return
				}
				user, pass, ok := r.BasicAuth()
				if r.Method != http.MethodDelete || !ok || user != "fixture-user" || pass != "fixture-password" {
					t.Error("direct ARI request lost its method or configured authentication")
				}
				w.Header().Set("Location", "/redirected")
				w.WriteHeader(status)
			}))
			defer server.Close()
			err := New(server.URL, "fixture-user", "fixture-password").Delete("/channels/fixture")
			if (err == nil) != (status >= 200 && status < 300) {
				t.Fatalf("status=%d err=%v", status, err)
			}
			if redirected.Load() != 0 {
				t.Fatal("ARI DELETE followed a redirect and could mistake its response for closure")
			}
		})
	}
}

func TestResourceHelpersRejectRedirectsWithValidJSONBodies(t *testing.T) {
	for _, operation := range []string{"inventory", "create"} {
		for _, status := range []int{200, 302} {
			t.Run(fmt.Sprintf("%s_%d", operation, status), func(t *testing.T) {
				body := "[]"
				if operation == "create" {
					body = `{"id":"fixture"}`
				}
				var redirected atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					if r.URL.Path == "/redirected" {
						redirected.Add(1)
						_, _ = fmt.Fprint(w, body)
						return
					}
					w.Header().Set("Location", "/redirected")
					w.WriteHeader(status)
					_, _ = fmt.Fprint(w, body)
				}))
				defer server.Close()
				client := New(server.URL, "", "")
				var err error
				if operation == "inventory" {
					_, err = client.LiveChannelIDs()
				} else {
					_, err = client.PostJSON("/bridges", map[string]string{"type": "mixing"})
				}
				if (err == nil) != (status == 200) {
					t.Fatalf("%s treated HTTP %d as successful resource data: %v", operation, status, err)
				}
				if redirected.Load() != 0 {
					t.Fatal("resource helper followed an unexpected redirect")
				}
			})
		}
	}
}

func TestRawHelpersReturnTheOriginalRedirectStatus(t *testing.T) {
	for _, operation := range []string{"get", "post", "params"} {
		t.Run(operation, func(t *testing.T) {
			var redirected atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/redirected" {
					redirected.Add(1)
					w.WriteHeader(200)
					return
				}
				w.Header().Set("Location", "/redirected")
				w.WriteHeader(302)
				_, _ = fmt.Fprint(w, `{"fixture":"original response"}`)
			}))
			defer server.Close()
			client := New(server.URL, "", "")
			var body []byte
			var status int
			var err error
			switch operation {
			case "get":
				body, status, err = client.Get("/channels")
			case "post":
				body, status, err = client.Post("/channels", map[string]string{"channelId": "fixture"})
			case "params":
				body, status, err = client.PostParams("/bridges/fixture/addChannel", url.Values{"channel": {"fixture"}})
			}
			if err != nil || status != 302 || string(body) != `{"fixture":"original response"}` || redirected.Load() != 0 {
				t.Fatalf("raw %s did not preserve the original response: status=%d body=%s err=%v redirects=%d", operation, status, body, err, redirected.Load())
			}
		})
	}
}
