package vad

import (
	"context"
	"math/rand"
	"testing"
	"time"

	"phoneagent/internal/rtp"
)

// The pre-roll is what carries the words spoken *before* the VAD trigger fires:
// triggering needs a 300ms mostly-voiced window, and dropping it cost the first
// word of every capture ("vas-y passe-le-moi" reached Whisper as "passe le mois",
// and the transfer was classified as a refusal).
func TestAppendPreRollKeepsNewestFrames(t *testing.T) {
	var pre [][]int16
	for i := 0; i < preRollFrames+10; i++ {
		pre = appendPreRoll(pre, []int16{int16(i)})
	}
	if len(pre) != preRollFrames {
		t.Fatalf("len(pre) = %d, want %d (the window must be bounded)", len(pre), preRollFrames)
	}
	// The window slides: it must end on the most recent frame and start
	// preRollFrames-1 earlier — the audio immediately preceding the trigger.
	if got, want := pre[len(pre)-1][0], int16(preRollFrames+9); got != want {
		t.Errorf("newest frame = %d, want %d", got, want)
	}
	if got, want := pre[0][0], int16(10); got != want {
		t.Errorf("oldest kept frame = %d, want %d", got, want)
	}
}

func TestCancelledCaptureDoesNotConsumeMoreAudio(t *testing.T) {
	p, err := New()
	if err != nil {
		t.Fatal(err)
	}
	source := &fakeSource{frames: [][]int16{make([]int16, rtp.FrameSamples)}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	seg, timedOut := p.GetSpeechSegmentTimeoutContext(ctx, source, time.Second)
	if seg != nil || timedOut || source.i != 0 {
		t.Fatal("cancelled capture consumed audio or reported silence")
	}
}

func TestAppendPreRollBelowCapacityKeepsEverything(t *testing.T) {
	var pre [][]int16
	for i := 0; i < 3; i++ {
		pre = appendPreRoll(pre, []int16{int16(i)})
	}
	if len(pre) != 3 {
		t.Fatalf("len(pre) = %d, want 3 (nothing to evict yet)", len(pre))
	}
	if pre[0][0] != 0 {
		t.Errorf("oldest frame = %d, want 0", pre[0][0])
	}
}

func TestVoicedFraction(t *testing.T) {
	tests := []struct {
		name string
		ring []bool
		want float64
	}{
		{"all voiced", []bool{true, true, true, true}, 1.0},
		{"none voiced", []bool{false, false}, 0.0},
		{"half voiced", []bool{true, false, true, false}, 0.5},
		{"empty ring", nil, 0.0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := voicedFraction(tt.ring); got != tt.want {
				t.Errorf("voicedFraction(%v) = %v, want %v", tt.ring, got, tt.want)
			}
		})
	}
}

func TestAppendRingIsBounded(t *testing.T) {
	var ring []bool
	for i := 0; i < ringFrames*2; i++ {
		ring = appendRing(ring, i%2 == 0, ringFrames)
	}
	if len(ring) != ringFrames {
		t.Fatalf("len(ring) = %d, want %d", len(ring), ringFrames)
	}
}

// ── Capture path (real webrtcvad, synthetic frames) ───────────────────────────

// fakeSource replays a scripted frame sequence, then blocks-as-timeout (nil).
type fakeSource struct {
	frames [][]int16
	i      int
}

func (f *fakeSource) ReadFrame(time.Duration) []int16 {
	if f.i >= len(f.frames) {
		return nil
	}
	frame := f.frames[f.i]
	f.i++
	return frame
}

// webrtcvad classifies loud non-stationary noise as speech and digital silence
// as non-speech (100% of frames, modes 2 and 3), which makes a deterministic
// capture-path test possible without recorded audio. The frames must keep
// changing: webrtcvad adapts to a stationary signal and stops calling it speech.
func voicedFrames(n int) [][]int16 {
	rng := rand.New(rand.NewSource(42))
	out := make([][]int16, n)
	for i := range out {
		f := make([]int16, rtp.FrameSamples)
		for j := range f {
			f[j] = int16(rng.Intn(16000) - 8000)
		}
		out[i] = f
	}
	return out
}

func silentFrames(n int) [][]int16 {
	out := make([][]int16, n)
	for i := range out {
		out[i] = make([]int16, rtp.FrameSamples)
	}
	return out
}

// This is the regression test for the lost first word: the capture must contain
// the audio that *preceded* the trigger, not just what came after it. Trigger
// needs ringFrames of voiced audio, so a capture without pre-roll is short by
// exactly that much.
func TestGetSpeechSegmentTimeoutIncludesPreRoll(t *testing.T) {
	p, err := New()
	if err != nil {
		t.Fatalf("vad.New: %v", err)
	}
	const voiced = 40
	seq := append(silentFrames(5), voicedFrames(voiced)...)
	seq = append(seq, silentFrames(ringFrames+5)...)

	seg, timedOut := p.GetSpeechSegmentTimeout(&fakeSource{frames: seq}, 0)
	if timedOut || seg == nil {
		t.Fatal("expected a captured segment")
	}
	// Count the audible frames rather than the total: trailing silence is captured
	// too, so a length check would pass even with the onset thrown away. Without
	// pre-roll the capture starts at the trigger frame and loses the ringFrames of
	// speech that caused the trigger — i.e. the caller's first word.
	if got := nonSilentFrames(seg); got != voiced {
		t.Errorf("captured %d audible frames out of %d spoken — the pre-trigger audio was dropped (lost first word)",
			got, voiced)
	}
}

// nonSilentFrames counts the 20ms frames of seg that carry any signal.
func nonSilentFrames(seg []int16) int {
	n := 0
	for i := 0; i+rtp.FrameSamples <= len(seg); i += rtp.FrameSamples {
		for _, s := range seg[i : i+rtp.FrameSamples] {
			if s != 0 {
				n++
				break
			}
		}
	}
	return n
}

// Echo keeps the caller stream voiced for as long as the agent speaks, so the
// validator must be consulted repeatedly *during* the utterance rather than at
// its end — otherwise a genuine interruption on an echoing line never fires.
func TestGetSpeechSegmentCtxRevalidatesWhileSpeechContinues(t *testing.T) {
	p, err := NewWithMode(3)
	if err != nil {
		t.Fatalf("vad.NewWithMode: %v", err)
	}
	sustain := 10 // frames
	seq := append(silentFrames(2), voicedFrames(sustain*6)...)
	seq = append(seq, silentFrames(ringFrames+5)...)

	// Bounded: a validator that never accepts would otherwise poll a dead stream.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	calls := 0
	seg := p.GetSpeechSegmentCtx(ctx, &fakeSource{frames: seq}, sustain,
		func(partial []int16) bool {
			calls++
			return calls >= 3 // reject twice (echo), accept on the third window
		})
	if calls < 3 {
		t.Errorf("validator called %d times, want ≥3 (it must be re-consulted mid-utterance)", calls)
	}
	if seg == nil {
		t.Fatal("a confirmed barge-in must return the captured utterance")
	}
}

// A rejected utterance (echo throughout) must not be consumed as a caller turn,
// and monitoring must keep running.
func TestGetSpeechSegmentCtxDiscardsAlwaysRejectedSpeech(t *testing.T) {
	p, err := NewWithMode(3)
	if err != nil {
		t.Fatalf("vad.NewWithMode: %v", err)
	}
	seq := append(silentFrames(2), voicedFrames(30)...)
	seq = append(seq, silentFrames(ringFrames+5)...)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled: after the rejected utterance, it must give up
	seg := p.GetSpeechSegmentCtx(ctx, &fakeSource{frames: seq}, 10, func([]int16) bool { return false })
	if seg != nil {
		t.Errorf("rejected speech must not be returned as a turn, got %d samples", len(seg))
	}
}

func TestCaptureStreamsSameAudioIncludingPreRollBeforeReturning(t *testing.T) {
	p, err := New()
	if err != nil {
		t.Fatal(err)
	}
	source := &fakeSource{frames: append(append(silentFrames(20), voicedFrames(40)...), silentFrames(40)...)}
	var streamed []int16
	firstRead := 0
	samples, timedOut := p.Capture(context.Background(), source, time.Second, func(frame []int16) {
		if firstRead == 0 {
			firstRead = source.i
		}
		streamed = append(streamed, frame...)
	})
	if timedOut || len(samples) == 0 || len(streamed) != len(samples) {
		t.Fatalf("streamed=%d captured=%d", len(streamed), len(samples))
	}
	for i := range samples {
		if streamed[i] != samples[i] {
			t.Fatal("streamed audio differs from captured utterance")
		}
	}
	if firstRead >= source.i {
		t.Fatal("upload started after capture finished")
	}
}
