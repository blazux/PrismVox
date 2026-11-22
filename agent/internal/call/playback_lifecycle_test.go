package call

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"phoneagent/internal/ari"
	"phoneagent/internal/audio"
)

func playbackFixture(t *testing.T) []byte {
	t.Helper()
	previous := soundsDir
	soundsDir = t.TempDir()
	t.Cleanup(func() { soundsDir = previous })
	wav, err := audio.EncodeWAV(make([]float32, 160), 16000)
	if err != nil {
		t.Fatal(err)
	}
	return wav
}

func TestPlaybackFinishedBeforeRESTReplyIsRetained(t *testing.T) {
	for _, queued := range []bool{false, true} {
		name := "single"
		if queued {
			name = "queued"
		}
		t.Run(name, func(t *testing.T) {
			wav := playbackFixture(t)
			h := NewHandler("caller", nil, nil, "")
			var registered atomic.Bool
			var starts atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost {
					w.WriteHeader(http.StatusNoContent)
					return
				}
				starts.Add(1)
				id := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, "/channels/recipient/play"), "/")
				if id == "" {
					id = "legacy-server-id"
				}
				h.pbMu.Lock()
				_, exists := h.pbFutures[id]
				h.pbMu.Unlock()
				registered.Store(exists)
				h.OnPlaybackFinished(id) // The WebSocket event wins the race with this REST reply.
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]string{"id": id})
			}))
			defer server.Close()
			h.ari = ari.New(server.URL, "", "")
			ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
			defer cancel()
			if queued {
				in := make(chan playChunk, 2)
				in <- playChunk{wav: wav}
				in <- playChunk{wav: wav}
				close(in)
				h.playQueued(ctx, "recipient", in)
			} else if err := h.playWAV(ctx, wav, "recipient"); err != nil {
				t.Fatal(err)
			}
			if ctx.Err() != nil || !registered.Load() {
				t.Fatalf("completion was lost: context=%v registered before POST=%v", ctx.Err(), registered.Load())
			}
			expected := int32(1)
			if queued {
				expected = 2
			}
			if starts.Load() != expected {
				t.Fatalf("play starts=%d, want %d", starts.Load(), expected)
			}
			h.pbMu.Lock()
			remaining := len(h.pbFutures)
			h.pbMu.Unlock()
			files, err := os.ReadDir(soundsDir)
			if err != nil || remaining != 0 || len(files) != 0 {
				t.Fatalf("playback resources remain: futures=%d files=%d err=%v", remaining, len(files), err)
			}
		})
	}
}

func TestPlaybackStartFailureReleasesFutureAndKnownPlayback(t *testing.T) {
	wav := playbackFixture(t)
	h := NewHandler("caller", nil, nil, "")
	var startedID, deletedID string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			startedID = strings.TrimPrefix(r.URL.Path, "/channels/recipient/play/")
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		if r.Method == http.MethodDelete {
			deletedID = strings.TrimPrefix(r.URL.Path, "/playbacks/")
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	h.ari = ari.New(server.URL, "", "")
	if err := h.playWAV(context.Background(), wav, "recipient"); err == nil {
		t.Fatal("play start failure was ignored")
	}
	h.pbMu.Lock()
	remaining := len(h.pbFutures)
	h.pbMu.Unlock()
	files, err := os.ReadDir(soundsDir)
	if startedID == "" || startedID != deletedID || remaining != 0 || len(files) != 0 || err != nil {
		t.Fatalf("playback failure leaked resources: started=%q deleted=%q futures=%d files=%d err=%v", startedID, deletedID, remaining, len(files), err)
	}
}

func TestPlaybackCancellationReleasesFutureAndFile(t *testing.T) {
	wav := playbackFixture(t)
	h := NewHandler("caller", nil, nil, "")
	started := make(chan struct{})
	var deleted atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			close(started)
			w.WriteHeader(http.StatusCreated)
			return
		}
		if r.Method == http.MethodDelete {
			deleted.Add(1)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	h.ari = ari.New(server.URL, "", "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- h.playWAV(ctx, wav, "recipient") }()
	<-started
	cancel()
	select {
	case err := <-done:
		if err != context.Canceled {
			t.Fatalf("cancelled playback returned %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled playback did not finish")
	}
	h.pbMu.Lock()
	remaining := len(h.pbFutures)
	h.pbMu.Unlock()
	files, err := os.ReadDir(soundsDir)
	if deleted.Load() != 1 || remaining != 0 || len(files) != 0 || err != nil {
		t.Fatalf("cancelled playback leaked: deletes=%d futures=%d files=%d err=%v", deleted.Load(), remaining, len(files), err)
	}
}
