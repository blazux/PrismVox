package stt

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"github.com/gorilla/websocket"
	"net/http"
	"net/http/httptest"
	"phoneagent/internal/voice"
	"reflect"
	"testing"
	"time"
)

func TestRealtimeUploadsWhileSpeakingAndCommitsExactlyOnce(t *testing.T) {
	uploaded := make(chan struct{}, 1)
	samples := make(chan []int16, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("xi-api-key") != "fixture" || r.URL.Query().Get("commit_strategy") != "manual" || r.URL.Query().Get("audio_format") != "pcm_16000" {
			t.Error("invalid realtime handshake")
		}
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		conn.WriteJSON(map[string]string{"message_type": "session_started"})
		var got []int16
		for {
			var ev struct {
				Audio  string `json:"audio_base_64"`
				Commit bool   `json:"commit"`
			}
			if conn.ReadJSON(&ev) != nil {
				return
			}
			data, err := base64.StdEncoding.DecodeString(ev.Audio)
			if err != nil {
				t.Error(err)
				return
			}
			for i := 0; i < len(data); i += 2 {
				got = append(got, int16(binary.LittleEndian.Uint16(data[i:])))
			}
			select {
			case uploaded <- struct{}{}:
			default:
			}
			if ev.Commit {
				samples <- got
				conn.WriteJSON(map[string]string{"message_type": "committed_transcript", "text": "Bonjour"})
				return
			}
			conn.WriteJSON(map[string]string{"message_type": "partial_transcript", "text": "Bon"})
		}
	}))
	defer server.Close()
	c := New()
	c.Configure(voice.Settings{STTProvider: "elevenlabs", STTModel: "scribe_v2_realtime", BaseURL: server.URL, APIKey: "fixture"})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	stream := c.Begin(ctx)
	defer stream.Close()
	pcm := make([]int16, 6400)
	for i := range pcm {
		pcm[i] = int16(i)
	}
	for i := 0; i < len(pcm); i += 320 {
		stream.Add(pcm[i : i+320])
	}
	select {
	case <-uploaded:
	case <-ctx.Done():
		t.Fatal("STT waited for utterance completion")
	}
	text, err := stream.Finish()
	if err != nil || text != "Bonjour" {
		t.Fatalf("%q %v", text, err)
	}
	if got := <-samples; !reflect.DeepEqual(got, pcm) {
		t.Fatalf("audio duplicated or truncated: %d samples", len(got))
	}
}

func TestRealtimeCancellationClosesWebSocket(t *testing.T) {
	started, stopped := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		close(started)
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				close(stopped)
				return
			}
		}
	}))
	defer server.Close()
	c := New()
	c.Configure(voice.Settings{STTProvider: "elevenlabs", STTModel: "scribe_v2_realtime", BaseURL: server.URL})
	ctx, cancel := context.WithCancel(context.Background())
	stream := c.Begin(ctx)
	defer stream.Close()
	<-started
	cancel()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("STT connection survived call hangup")
	}
}

func TestLegacyServiceReceivesDictionaryAndWAV(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Error(err)
			return
		}
		if r.FormValue("hotwords") != "Digicel, BroadWorks" {
			t.Error("dictionary was not forwarded")
		}
		file, _, err := r.FormFile("audio")
		if err != nil {
			t.Error(err)
			return
		}
		file.Close()
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"text":"Bonjour"}`))
	}))
	defer server.Close()
	t.Setenv("STT_URL", server.URL)
	c := New()
	c.Hotwords = "Digicel, BroadWorks"
	text, err := c.Transcribe(context.Background(), []byte("fixture WAV"))
	if err != nil || text != "Bonjour" {
		t.Fatalf("%q %v", text, err)
	}
}
