package tts

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	maxAudioBytes = 8 << 20
	maxCacheBytes = 256 << 20
	maxCacheFiles = 4096
)

// All callers in the agent share locks; different calls never duplicate a cold
// synthesis. Waiting is cancellable, and failed/interrupted audio is not stored.
var audioLocks = struct {
	sync.Mutex
	entries map[string]*audioLock
}{entries: make(map[string]*audioLock)}

type audioLock struct {
	ready chan struct{}
	users int
}

func lockAudio(ctx context.Context, key string) (func(), error) {
	audioLocks.Lock()
	l := audioLocks.entries[key]
	if l == nil {
		l = &audioLock{ready: make(chan struct{}, 1)}
		l.ready <- struct{}{}
		audioLocks.entries[key] = l
	}
	l.users++
	audioLocks.Unlock()
	release := func(held bool) {
		if held {
			l.ready <- struct{}{}
		}
		audioLocks.Lock()
		l.users--
		if l.users == 0 {
			delete(audioLocks.entries, key)
		}
		audioLocks.Unlock()
	}
	select {
	case <-ctx.Done():
		release(false)
		return nil, ctx.Err()
	case <-l.ready:
		return func() { release(true) }, nil
	}
}

func (c *Client) cachePath(text, format string) string {
	if c.cacheDir == "" {
		return ""
	}
	// Hash the effective request, including account identity, without writing
	// credentials, spoken text or provider request IDs to filenames or metadata.
	endpoint, model, voice, credential := c.URL, c.settings.ServiceTTSModel, c.settings.ServiceVoice, c.settings.ServiceAPIKey
	prepared := text
	if c.settings.TTSProvider == "elevenlabs" {
		endpoint, model, voice, credential = c.settings.BaseURL, c.settings.TTSModel, c.settings.VoiceID, c.settings.APIKey
		prepared = c.prepareText(text)
	} else if c.settings.TTSProvider == "openai" {
		prepared = c.prepareText(text)
	}
	key, _ := json.Marshal(struct {
		Version, Format, Provider, Endpoint, Model, Voice, Language string
		Text, Prepared, SpellWords, SpellExceptions, Credential     string
		Stability                                                   *float64
	}{"1", format, c.settings.TTSProvider, endpoint, model, voice, c.settings.Language,
		text, prepared, c.SpellWords, c.SpellExceptions, credential, c.settings.Stability})
	hash := sha256.Sum256(key)
	return filepath.Join(c.cacheDir, hex.EncodeToString(hash[:])+".audio")
}

func readAudio(path string) ([]byte, bool) {
	f, err := os.Open(path)
	if err != nil {
		return nil, false
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxAudioBytes+sha256.Size+1))
	if err != nil || len(data) <= sha256.Size || len(data) > maxAudioBytes+sha256.Size {
		return nil, false
	}
	payload := data[sha256.Size:]
	hash := sha256.Sum256(payload)
	if !bytes.Equal(hash[:], data[:sha256.Size]) {
		return nil, false
	}
	now := time.Now()
	_ = os.Chtimes(path, now, now)
	return payload, true
}

var cacheWriteMu sync.Mutex

func writeAudio(path string, data []byte) {
	if len(data) == 0 || len(data) > maxAudioBytes {
		return
	}
	cacheWriteMu.Lock()
	defer cacheWriteMu.Unlock()
	dir := filepath.Dir(path)
	if os.MkdirAll(dir, 0700) != nil {
		return // Cache availability must never prevent speech.
	}
	if os.Chmod(dir, 0700) != nil {
		return
	}
	f, err := os.CreateTemp(dir, ".audio-*")
	if err != nil {
		return
	}
	defer os.Remove(f.Name())
	hash := sha256.Sum256(data)
	_, err = f.Write(hash[:])
	if err == nil {
		_, err = f.Write(data)
	}
	closeErr := f.Close()
	if err != nil || closeErr != nil || os.Rename(f.Name(), path) != nil {
		return
	}
	pruneAudio(dir, maxCacheBytes, maxCacheFiles)
}

func pruneAudio(dir string, maxBytes int64, maxFiles int) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	type item struct {
		name string
		info os.FileInfo
	}
	var files []item
	var size int64
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".audio") {
			continue
		}
		info, err := entry.Info()
		if err == nil {
			files = append(files, item{entry.Name(), info})
			size += info.Size()
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].info.ModTime().Before(files[j].info.ModTime()) })
	remaining := len(files)
	for _, file := range files {
		if size <= maxBytes && remaining <= maxFiles {
			break
		}
		if os.Remove(filepath.Join(dir, file.name)) == nil {
			size -= file.info.Size()
			remaining--
		}
	}
}

// streamCached keeps the cold path streaming. Only a fully consumed synthesis
// is published atomically; replay uses the same small PCM chunks/backpressure.
func (c *Client) streamCached(ctx context.Context, text string, emit func([]int16) error) error {
	path := c.cachePath(text, "pcm8000")
	if path == "" {
		return c.streamElevenLabs(ctx, text, emit)
	}
	if data, ok := readAudio(path); ok && len(data)%2 == 0 {
		return c.replayAudio(ctx, text, data, emit)
	}
	unlock, err := lockAudio(ctx, path)
	if err != nil {
		return err
	}
	if data, ok := readAudio(path); ok && len(data)%2 == 0 {
		unlock()
		return c.replayAudio(ctx, text, data, emit)
	}
	defer unlock()
	var data []byte
	cacheable := true
	err = c.streamElevenLabs(ctx, text, func(samples []int16) error {
		if cacheable && len(data)+len(samples)*2 <= maxAudioBytes {
			for _, sample := range samples {
				data = binary.LittleEndian.AppendUint16(data, uint16(sample))
			}
		} else {
			cacheable, data = false, nil
		}
		return emit(samples)
	})
	if err == nil && ctx.Err() == nil && cacheable {
		writeAudio(path, data)
	}
	return err
}

func (c *Client) replayAudio(ctx context.Context, text string, data []byte, emit func([]int16) error) error {
	for offset := 0; offset < len(data); offset += 1600 {
		if err := ctx.Err(); err != nil {
			return err
		}
		end := min(offset+1600, len(data))
		samples := make([]int16, (end-offset)/2)
		for i := range samples {
			samples[i] = int16(binary.LittleEndian.Uint16(data[offset+2*i:]))
		}
		if err := emit(samples); err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	c.rememberCachedSpeech(c.prepareText(text))
	return nil
}
