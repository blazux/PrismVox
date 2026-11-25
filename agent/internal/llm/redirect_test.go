package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestModelRedirectKeepsPrivateRequestsAtConfiguredOrigin(t *testing.T) {
	for _, code := range []int{307, 308} {
		for _, sameOrigin := range []bool{false, true} {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("status%d/sameOrigin%t/stream%t", code, sameOrigin, stream), func(t *testing.T) {
					var foreign atomic.Int32
					answer := func(w http.ResponseWriter, r *http.Request) {
						var body map[string]interface{}
						if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
							t.Error(err)
						}
						if r.Header.Get("Authorization") != "Bearer fixture-key" && sameOrigin {
							t.Error("same-origin redirect lost authentication")
						}
						if stream {
							w.Header().Set("Content-Type", "text/event-stream")
							fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Synthetic reply.\"}}]}\n\ndata: [DONE]\n\n")
						} else {
							fmt.Fprint(w, `{"choices":[{"message":{"content":"Synthetic reply."}}]}`)
						}
					}
					target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						foreign.Add(1)
						answer(w, r)
					}))
					defer target.Close()
					source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if r.URL.Path == "/canonical" {
							answer(w, r)
							return
						}
						location := target.URL
						if sameOrigin {
							location = "/canonical"
						}
						http.Redirect(w, r, location, code)
					}))
					defer source.Close()
					t.Setenv("OPENAI_BASE_URL", source.URL)
					t.Setenv("OPENAI_API_KEY", "fixture-key")
					c := New()
					var err error
					if stream {
						sentences, tools, failures := c.StreamResponse(context.Background(), "Private fixture", nil, "", "", nil, "Private user text")
						for range sentences {
						}
						for range tools {
						}
						err = <-failures
					} else {
						_, _, err = c.GetResponse(context.Background(), "Private fixture", nil, "", "", nil, "Private user text")
					}
					if (err == nil) != sameOrigin || foreign.Load() != 0 {
						t.Fatalf("sameOrigin=%t error=%v foreignRequests=%d", sameOrigin, err, foreign.Load())
					}
				})
			}
		}
	}
}
