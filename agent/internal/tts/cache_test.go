package tts

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"phoneagent/internal/audio"
	"phoneagent/internal/voice"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	// Unit tests never read or write the deployment's persistent cache.
	os.Setenv("TTS_CACHE_DIR", "off")
	os.Exit(m.Run())
}

func cachedClient(dir, endpoint string) *Client {
	c := New()
	c.cacheDir = dir
	c.Configure(voice.Settings{TTSProvider: "elevenlabs", BaseURL: endpoint, VoiceID: "fixture", TTSModel: "eleven_flash_v2_5", Language: "fr"})
	return c
}

func collectSpeech(c *Client, text string) ([]int16, error) {
	var result []int16
	err := c.Stream(context.Background(), text, func(pcm []int16) error {
		result = append(result, pcm...)
		return nil
	})
	return result, err
}

func TestAudioPersistsAcrossClientsAndProviderUnavailability(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("request-id", "private-request-id")
		w.Write([]byte{0x34, 0x12, 0xff, 0xff})
	}))
	defer server.Close()
	dir := t.TempDir()
	c := cachedClient(dir, server.URL)
	c.settings.APIKey = "private-key"
	want := []int16{0x1234, -1}
	got, err := collectSpeech(c, "Bonjour Vincent.")
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("cold audio: %v, %v", got, err)
	}
	server.Close()
	other := cachedClient(dir, server.URL)
	other.settings.APIKey = c.settings.APIKey
	got, err = collectSpeech(other, "Bonjour Vincent.")
	if err != nil || !reflect.DeepEqual(got, want) || requests.Load() != 1 {
		t.Fatalf("persisted audio: %v, %v; requests=%d", got, err, requests.Load())
	}
	files, _ := os.ReadDir(dir)
	if len(files) != 1 || files[0].Name() == "Bonjour Vincent." {
		t.Fatal("unexpected cache files")
	}
	path := filepath.Join(dir, files[0].Name())
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0600 {
		t.Fatal("audio cache is not private")
	}
	data, _ := os.ReadFile(path)
	for _, secret := range []string{"private-key", "private-request-id", "Bonjour Vincent."} {
		if bytes.Contains(data, []byte(secret)) {
			t.Fatal("cache retained request metadata")
		}
	}
}

func TestCacheSeparatesEffectiveVoiceSettingsAndAccounts(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Write([]byte{1, 0})
	}))
	defer server.Close()
	dir := t.TempDir()
	zero := 0.0
	variants := []func(*Client){
		func(c *Client) {},
		func(c *Client) { c.settings.VoiceID = "other-voice" },
		func(c *Client) { c.settings.TTSModel = "other-model" },
		func(c *Client) { c.settings.Language = "en" },
		func(c *Client) { c.settings.AutoLanguage = true },
		func(c *Client) { c.settings.Stability = &zero },
		func(c *Client) { c.settings.APIKey = "other-account" },
		func(c *Client) { c.settings.BaseURL += "/other-endpoint" },
		func(c *Client) { c.SpellWords = "SIP" },
		func(c *Client) { c.SpellExceptions = "SIP" },
	}
	for i, change := range variants {
		c := cachedClient(dir, server.URL)
		change(c)
		for repeat := 0; repeat < 2; repeat++ {
			if _, err := collectSpeech(c, "Bonjour SIP."); err != nil {
				t.Fatal(err)
			}
		}
		if requests.Load() != int32(i+1) {
			t.Fatalf("configuration %d incorrectly reused audio; requests=%d", i, requests.Load())
		}
	}
	c := cachedClient(dir, server.URL)
	if _, err := collectSpeech(c, "Bonjour Paul."); err != nil || requests.Load() != int32(len(variants)+1) {
		t.Fatal("different text reused audio", err)
	}
}

func TestConcurrentColdCallsShareSynthesisAndWaitersCanCancel(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) == 1 {
			close(started)
		}
		<-release
		w.Write([]byte{1, 0})
	}))
	defer server.Close()
	dir := t.TempDir()
	errorsCh := make(chan error, 12)
	go func() { _, err := collectSpeech(cachedClient(dir, server.URL), "Bonjour."); errorsCh <- err }()
	<-started
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := cachedClient(dir, server.URL).Stream(ctx, "Bonjour.", func([]int16) error { return nil }); !errors.Is(err, context.Canceled) {
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal("cancelled waiter did not stop", err)
		}
	}
	var wg sync.WaitGroup
	for i := 0; i < 11; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := collectSpeech(cachedClient(dir, server.URL), "Bonjour.")
			errorsCh <- err
		}()
	}
	close(release)
	wg.Wait()
	for i := 0; i < 12; i++ {
		if err := <-errorsCh; err != nil {
			t.Fatal(err)
		}
	}
	if requests.Load() != 1 {
		t.Fatalf("duplicate cold synthesis: %d requests", requests.Load())
	}
}

func TestInterruptedFailedAndCorruptAudioIsNeverReused(t *testing.T) {
	for _, mode := range []string{"interrupted", "http-failure", "odd-pcm", "truncated-pcm", "corrupt-cache"} {
		t.Run(mode, func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				n := requests.Add(1)
				if n == 1 && mode == "http-failure" {
					w.WriteHeader(503)
					return
				}
				if n == 1 && mode == "odd-pcm" {
					w.Write([]byte{1})
					return
				}
				if n == 1 && mode == "truncated-pcm" {
					w.Header().Set("Content-Length", "4")
				}
				w.Write([]byte{1, 0})
			}))
			defer server.Close()
			c := cachedClient(t.TempDir(), server.URL)
			stop := errors.New("interrupted")
			err := c.Stream(context.Background(), "Bonjour.", func([]int16) error {
				if mode == "interrupted" {
					return stop
				}
				return nil
			})
			if mode == "corrupt-cache" {
				if err != nil {
					t.Fatal(err)
				}
				os.WriteFile(c.cachePath("Bonjour.", "pcm8000"), []byte("corrupted audio"), 0600)
			} else if err == nil {
				t.Fatal("incomplete synthesis succeeded")
			}
			for i := 0; i < 2; i++ {
				if _, err := collectSpeech(c, "Bonjour."); err != nil {
					t.Fatal(err)
				}
			}
			if requests.Load() != 2 {
				t.Fatalf("failed audio reused or repair not cached: %d", requests.Load())
			}
		})
	}
}

func TestCachedPlaybacksDoNotWaitForAnotherCallsPlayback(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte{1, 0}) }))
	defer server.Close()
	dir := t.TempDir()
	if _, err := collectSpeech(cachedClient(dir, server.URL), "Bonjour."); err != nil {
		t.Fatal(err)
	}
	server.Close()
	started, release := make(chan struct{}), make(chan struct{})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- cachedClient(dir, server.URL).Stream(ctx, "Bonjour.", func([]int16) error {
			close(started)
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	}()
	<-started
	if err := cachedClient(dir, server.URL).Stream(ctx, "Bonjour.", func([]int16) error { return nil }); err != nil {
		t.Fatal("second call waited for the first caller's playback", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestCacheDoesNotDelayColdStreamAndReplaysRespectInterruption(t *testing.T) {
	first, finish := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(make([]byte, 1600))
		w.(http.Flusher).Flush()
		<-finish
		w.Write(make([]byte, 1600))
	}))
	defer server.Close()
	c := cachedClient(t.TempDir(), server.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		var once sync.Once
		done <- c.Stream(ctx, "Bonjour.", func([]int16) error { once.Do(func() { close(first) }); return nil })
	}()
	select {
	case <-first:
	case <-ctx.Done():
		close(finish)
		t.Fatal("cache buffered the cold synthesis before playback")
	}
	close(finish)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	stop := errors.New("caller spoke")
	chunks := 0
	if err := c.Stream(context.Background(), "Bonjour.", func([]int16) error { chunks++; return stop }); !errors.Is(err, stop) || chunks != 1 {
		t.Fatal("cached playback ignored interruption", err, chunks)
	}
	ctx, cancel = context.WithCancel(context.Background())
	cancel()
	if err := c.Stream(ctx, "Bonjour.", func([]int16) error { t.Error("played cancelled audio"); return nil }); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestCachedAudioUsesOnlyCurrentCallsTextForContinuity(t *testing.T) {
	var bodies []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		bodies = append(bodies, body)
		w.Header().Set("request-id", "other-call-request")
		w.Write([]byte{1, 0})
	}))
	defer server.Close()
	dir := t.TempDir()
	if _, err := collectSpeech(cachedClient(dir, server.URL), "Bonjour."); err != nil {
		t.Fatal(err)
	}
	c := cachedClient(dir, server.URL)
	if _, err := collectSpeech(c, "Bonjour."); err != nil {
		t.Fatal(err)
	}
	if _, err := collectSpeech(c, "Comment allez-vous ?"); err != nil {
		t.Fatal(err)
	}
	if len(bodies) != 2 || bodies[1]["previous_text"] != "Bonjour." || bodies[1]["previous_request_ids"] != nil {
		t.Fatalf("cached speech leaked another call's stitching IDs: %+v", bodies)
	}
	if _, err := collectSpeech(c, "Très bien."); err != nil {
		t.Fatal(err)
	}
	if bodies[2]["previous_text"] != nil || bodies[2]["previous_request_ids"] == nil {
		t.Fatal("fresh request continuity did not resume")
	}
}

func TestBatchCachePreservesWAVFormatAndMissingCacheDoesNotStopSpeech(t *testing.T) {
	wav, _ := audio.EncodeWAV(make([]float32, 1600), 16000)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Write(wav)
	}))
	defer server.Close()
	c := New()
	c.cacheDir = t.TempDir()
	c.Configure(voice.Settings{TTSProvider: "openai", TTSURL: server.URL, ServiceTTSModel: "fixture", ServiceVoice: "voice"})
	for i := 0; i < 2; i++ {
		got, err := c.SynthesizeContext(context.Background(), "Bonjour.")
		if err != nil || !bytes.Equal(got, wav) {
			t.Fatal("batch cache changed audio", err)
		}
	}
	if requests.Load() != 1 {
		t.Fatal("batch speech was not reused")
	}
	blocked := filepath.Join(t.TempDir(), "not-a-directory")
	os.WriteFile(blocked, []byte("fixture"), 0600)
	c.cacheDir = filepath.Join(blocked, "audio")
	for i := 0; i < 2; i++ {
		if _, err := c.SynthesizeContext(context.Background(), "Bonjour."); err != nil {
			t.Fatal("unavailable cache prevented speech", err)
		}
	}
	if requests.Load() != 3 {
		t.Fatal("invalid cache fixture")
	}
}

func TestServiceVoiceSelectionRefreshesPersistentAudio(t *testing.T) {
	first, _ := audio.EncodeWAV([]float32{0.1, 0.1}, 8000)
	second, _ := audio.EncodeWAV([]float32{0.2, 0.2}, 8000)
	var requests atomic.Int32
	var switched atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if switched.Load() {
			w.Write(second)
		} else {
			w.Write(first)
		}
	}))
	defer server.Close()
	dir := t.TempDir()
	speak := func(selected string, want []byte) {
		t.Helper()
		c := New()
		c.cacheDir = dir
		c.Configure(voice.Settings{TTSProvider: "service", TTSURL: server.URL, ServiceVoice: selected})
		got, err := c.SynthesizeContext(context.Background(), "Bonjour.")
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("voice %q returned stale or invalid audio: %v", selected, err)
		}
	}
	speak("", first)
	switched.Store(true)
	speak("new-voice.wav", second)
	speak("new-voice.wav", second)
	if requests.Load() != 2 {
		t.Fatalf("expected one synthesis per selected voice, got %d", requests.Load())
	}
}

func TestCacheEvictionKeepsRecentlyUsedAudioAndBoundsStorage(t *testing.T) {
	dir := t.TempDir()
	for i, name := range []string{"a", "b", "c"} {
		path := filepath.Join(dir, name+".audio")
		writeAudio(path, []byte{1, 0})
		old := time.Now().Add(time.Duration(i-10) * time.Hour)
		os.Chtimes(path, old, old)
	}
	readAudio(filepath.Join(dir, "a.audio"))
	pruneAudio(dir, 1000, 2)
	if _, err := os.Stat(filepath.Join(dir, "b.audio")); !os.IsNotExist(err) {
		t.Fatal("oldest unused audio was not evicted")
	}
	if _, ok := readAudio(filepath.Join(dir, "a.audio")); !ok {
		t.Fatal("recently used audio was evicted")
	}
	pruneAudio(dir, 34, 2)
	files, _ := os.ReadDir(dir)
	if len(files) != 1 {
		t.Fatal("byte limit was not respected")
	}
}
