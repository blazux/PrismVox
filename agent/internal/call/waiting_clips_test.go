package call

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"phoneagent/internal/audio"
	"phoneagent/internal/llm"
)

func TestWaitingRotationAvoidsRecentClipsAcrossCalls(t *testing.T) {
	r := waitingRotation{}
	clips := []waitingClip{{key: "a"}, {key: "b"}, {key: "c"}, {key: "d"}, {key: "e"}}
	var recent []string
	for turn := 0; turn < 100; turn++ {
		got := r.choose(clips, false)
		for _, previous := range recent {
			if got.key == previous {
				t.Fatalf("recent filler repeated on turn %d: %q", turn, got.key)
			}
		}
		recent = append(recent, got.key)
		if len(recent) > 3 {
			recent = recent[1:]
		}
	}
}

func TestWaitingRotationRestrictsConsultationClipsAndHandlesSmallPacks(t *testing.T) {
	r := waitingRotation{}
	clips := []waitingClip{{key: "neutral"}, {key: "lookup", LookupOnly: true}}
	for i := 0; i < 10; i++ {
		if got := r.choose(clips, false); got.key != "neutral" {
			t.Fatal("announced a consultation without a running lookup", got)
		}
	}
	if got := r.choose(clips, true); got.key != "lookup" {
		t.Fatal("consultation variant not available during lookup", got)
	}
	if got := r.choose(clips[1:], false); got.key != "" {
		t.Fatal("lookup-only pack produced a misleading filler")
	}
}

func TestWaitingLookupTracksActualRemoteAndLocalExecution(t *testing.T) {
	c := &waitingCue{}
	start := llm.ToolTrace{Type: "tool_use", ID: "read", Tool: "calendar", Input: map[string]interface{}{"action": "list"}}
	c.observeTool(start)
	c.observeTool(start)
	c.observeTool(llm.ToolTrace{Type: "tool_use", ID: "write", Tool: "calendar", Input: map[string]interface{}{"action": "add"}})
	if c.lookups.Load() != 1 {
		t.Fatal("lookup activity duplicated or included a write")
	}
	c.observeTool(llm.ToolTrace{Type: "tool_result", ID: "read"})
	c.observeTool(llm.ToolTrace{Type: "tool_result", ID: "read"})
	c.observeTool(start) // a late replay cannot resurrect a completed lookup
	if c.lookups.Load() != 0 {
		t.Fatal("completed lookup remained active")
	}
	finish := c.beginLookup("search_knowledge_base", nil)
	if c.lookups.Load() != 1 {
		t.Fatal("local lookup was not observed")
	}
	finish()
	if c.lookups.Load() != 0 {
		t.Fatal("local lookup survived its result")
	}
	for _, tc := range []struct {
		name   string
		action string
	}{{"calendar", "add"}, {"email", "send"}, {"end_call", "list"}, {"exec_command", ""}} {
		if lookupCueEligible(tc.name, map[string]interface{}{"action": tc.action}) {
			t.Fatal("write or unknown tool classified as a consultation", tc)
		}
	}
}

func TestWaitingCueUsesPackWithoutAnnouncingCompletedLookups(t *testing.T) {
	c := &waitingCue{started: time.Now(), delay: time.Millisecond, clips: []waitingClip{
		{key: "neutral-integration", Text: "Alors…", wav: []byte("neutral")},
		{key: "lookup-integration", Text: "Je regarde…", wav: []byte("lookup"), LookupOnly: true},
	}}
	c.observeTool(llm.ToolTrace{Type: "tool_use", ID: "read", Tool: "calendar", Input: map[string]interface{}{"action": "list"}})
	c.observeTool(llm.ToolTrace{Type: "tool_result", ID: "read"})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	in := make(chan playChunk)
	out := (&Handler{responseCue: c}).withWaitCue(ctx, in)
	select {
	case clip := <-out:
		if clip.text != "Alors…" || string(clip.wav) != "neutral" {
			t.Fatal("completed consultation or legacy audio selected instead of neutral pack", clip.text)
		}
	case <-ctx.Done():
		t.Fatal("pack did not supply the waiting cue")
	}
	close(in)
	for range out {
		t.Fatal("more than one filler was played")
	}
}

func TestWaitingRotationIsSafeAcrossConcurrentCalls(t *testing.T) {
	r := waitingRotation{}
	clips := []waitingClip{{key: "a"}, {key: "b"}, {key: "c"}, {key: "d"}}
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				if got := r.choose(clips, false); got.key == "" {
					t.Error("concurrent call received no filler")
				}
			}
		}()
	}
	wg.Wait()
}

func TestLoadWaitingClipsAndLegacyFallback(t *testing.T) {
	dir := t.TempDir()
	wav, _ := audio.EncodeWAV(make([]float32, 800), 8000)
	os.WriteFile(filepath.Join(dir, "neutral.wav"), wav, 0600)
	os.WriteFile(filepath.Join(dir, "lookup.wav"), wav, 0600)
	writeManifest := func(clips []waitingClip) {
		t.Helper()
		data, _ := json.Marshal(clips)
		os.WriteFile(filepath.Join(dir, "clips.json"), data, 0600)
	}
	valid := []waitingClip{{File: "neutral.wav", Text: "Alors…"}, {File: "lookup.wav", Text: "Je regarde…", LookupOnly: true}}
	writeManifest(valid)
	clips, err := loadWaitingClips(dir)
	if err != nil || len(clips) != 2 || len(clips[0].wav) == 0 || !clips[1].LookupOnly {
		t.Fatal("valid audio pack failed to load", clips, err)
	}
	t.Setenv("WAIT_CUE_DIR", dir)
	t.Setenv("WAIT_CUE_WAV", filepath.Join(dir, "neutral.wav"))
	h := &Handler{}
	h.loadWaitCue()
	if len(h.waitCues) != 2 || len(h.waitCueWav) == 0 {
		t.Fatal("pack or legacy fallback was lost")
	}
	for _, bad := range [][]waitingClip{
		{{File: "../neutral.wav", Text: "outside"}},
		{{File: "missing.wav", Text: "missing"}},
		{{File: "neutral.wav", Text: "only lookup", LookupOnly: true}},
		{{File: "neutral.wav", Text: "a"}, {File: "neutral.wav", Text: "duplicate"}},
	} {
		writeManifest(bad)
		if _, err := loadWaitingClips(dir); err == nil {
			t.Fatal("invalid pack accepted", bad)
		}
	}
	h = &Handler{}
	h.loadWaitCue()
	if len(h.waitCues) != 0 || len(h.waitCueWav) == 0 {
		t.Fatal("invalid pack prevented the existing single-clip fallback")
	}
}
