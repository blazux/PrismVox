package call

import (
	"context"
	"testing"
	"time"

	"phoneagent/internal/rtp"
)

func TestWaitingCueOnlyBeforeFirstResponseAudio(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cue := &waitingCue{wav: []byte("cached"), started: time.Now(), delay: 20 * time.Millisecond}
	h := &Handler{responseCue: cue}
	in := make(chan playChunk, 2)
	in <- playChunk{pcm: []int16{1}, text: "Début."}
	out := h.withWaitCue(ctx, in)
	if got := <-out; len(got.pcm) != 1 {
		t.Fatal("fast reply was delayed by a cue")
	}
	// A later pause in synthesis must never insert a hesitation mid-response.
	time.Sleep(40 * time.Millisecond)
	in <- playChunk{pcm: []int16{2}, text: "Suite."}
	close(in)
	if got := <-out; len(got.pcm) != 1 || got.pcm[0] != 2 {
		t.Fatal("cue inserted between spoken chunks")
	}
	if _, ok := <-out; ok {
		t.Fatal("extra audio after the answer")
	}
	if cue.played.Load() {
		t.Fatal("fast response played a waiting cue")
	}
}

func TestWaitingCuePlaysOnceForSlowInitialResponse(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	cue := &waitingCue{wav: []byte("cached"), started: time.Now(), delay: 15 * time.Millisecond}
	h := &Handler{responseCue: cue}
	in := make(chan playChunk, 2)
	out := h.withWaitCue(ctx, in)
	select {
	case got := <-out:
		if string(got.wav) != "cached" || got.text != "Hmm…" {
			t.Fatal("missing cached cue")
		}
		if time.Since(cue.started) < cue.delay {
			t.Fatal("cue played before the deadline")
		}
	case <-ctx.Done():
		t.Fatal("slow reply never received its cue")
	}
	in <- playChunk{pcm: []int16{1}, text: "Réponse."}
	close(in)
	if got := <-out; len(got.pcm) != 1 {
		t.Fatal("answer did not follow the cue")
	}
	if _, ok := <-out; ok {
		t.Fatal("repeated cue")
	}
	// A tool's follow-up belongs to the same response and must not repeat it.
	next := make(chan playChunk, 1)
	next <- playChunk{pcm: []int16{2}}
	close(next)
	for got := range h.withWaitCue(ctx, next) {
		if len(got.wav) > 0 {
			t.Fatal("repeated cue in follow-up")
		}
	}
}

func TestWaitingCueCancelledAndEmptyResponsesStaySilent(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		cue := &waitingCue{wav: []byte("cached"), started: time.Now(), delay: time.Second}
		in := make(chan playChunk)
		if cancelled {
			cancel()
		} else {
			close(in)
		}
		out := (&Handler{responseCue: cue}).withWaitCue(ctx, in)
		select {
		case _, ok := <-out:
			if ok {
				t.Fatal("cancelled or empty reply played audio")
			}
		case <-time.After(time.Second):
			t.Fatal("cue pipeline did not stop")
		}
		cancel()
		if cue.played.Load() {
			t.Fatal("cue survived cancellation/end of stream")
		}
	}
}

func TestWaitingCuePrefersAudioAlreadyReadyAtDeadline(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h := &Handler{responseCue: &waitingCue{wav: []byte("cached"), started: time.Now().Add(-time.Second), delay: time.Millisecond}}
	in := make(chan playChunk, 1)
	in <- playChunk{pcm: []int16{1}}
	close(in)
	for got := range h.withWaitCue(ctx, in) {
		if len(got.wav) > 0 {
			t.Fatal("cue overtook ready response audio")
		}
	}
}

func TestWaitingCueKeepsOriginalDeadlineForToolFollowup(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	h := &Handler{responseCue: &waitingCue{wav: []byte("cached"), started: time.Now().Add(-time.Second), delay: time.Millisecond}}
	empty := make(chan playChunk)
	close(empty)
	for range h.withWaitCue(ctx, empty) {
		t.Fatal("empty model stream produced audio")
	}
	pending := make(chan playChunk)
	followup := h.withWaitCue(ctx, pending)
	select {
	case got := <-followup:
		if len(got.wav) == 0 {
			t.Fatal("tool follow-up lost its initial wait")
		}
	case <-ctx.Done():
		t.Fatal("deadline restarted after tool execution")
	}
}

func TestLocalToolCompletionDisarmsPendingWaitingCue(t *testing.T) {
	cue := &waitingCue{wav: []byte("cached"), started: time.Now(), delay: time.Second}
	h := &Handler{responseCue: cue, rtpReceiver: &rtp.Receiver{}}
	finish := h.localWaitCue(context.Background(), true)
	finish()
	if cue.played.Load() {
		t.Fatal("completed local tool left a pending waiting cue")
	}
}
