package call

import (
	"context"
	"log"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// One shared state follows a response through model generation, local tool work
// and its spoken follow-up. The first real audio permanently disarms the cue.
type waitingCue struct {
	wav           []byte
	started       time.Time
	delay         time.Duration
	spoken        atomic.Bool
	played        atomic.Bool
	clips         []waitingClip
	selectClips   func() []waitingClip
	lookups       atomic.Int32
	lookupMu      sync.Mutex
	remoteLookups map[string]bool
}

func (h *Handler) loadWaitCue() {
	if dir := os.Getenv("WAIT_CUE_DIR"); dir != "" {
		clips, err := loadWaitingClips(dir)
		if err == nil {
			h.waitCues = clips
		} else {
			log.Printf("[%s] Waiting clips unavailable: %v", h.channelID, err)
		}
	}
	path := os.Getenv("WAIT_CUE_WAV")
	if path == "" {
		return
	}
	wav, err := os.ReadFile(path)
	if err == nil {
		_, err = wavToEgressPCM(wav)
	}
	if err != nil {
		log.Printf("[%s] Waiting cue unavailable: %v", h.channelID, err)
		return
	}
	h.waitCueWav = wav
}

func (c *waitingCue) remaining() time.Duration {
	return c.delay - time.Since(c.started)
}

// withWaitCue merges the cached clip into the ordinary playback queue, never
// directly over speech. Fast replies and later gaps between chunks get no cue.
// Once queued, the full cue precedes ready response audio; only caller/call
// cancellation may flush it. Provider readiness must not cancel its playback.
func (h *Handler) withWaitCue(ctx context.Context, in <-chan playChunk) <-chan playChunk {
	c := h.responseCue
	if c == nil || c.spoken.Load() || c.played.Load() {
		return in
	}
	out := make(chan playChunk)
	go func() {
		defer close(out)
		timer := time.NewTimer(c.remaining())
		defer timer.Stop()
		deadline := timer.C
		send := func(chunk playChunk) bool {
			if ctx.Err() != nil {
				return false
			}
			select {
			case out <- chunk:
				return true
			case <-ctx.Done():
				return false
			}
		}
		realAudio := func(chunk playChunk) bool {
			if len(chunk.pcm) == 0 && len(chunk.wav) == 0 {
				return true
			}
			c.spoken.Store(true)
			deadline = nil
			timer.Stop()
			return send(chunk)
		}
		for {
			// Prefer audio already available when the deadline is reached.
			select {
			case chunk, ok := <-in:
				if !ok {
					return
				}
				if !realAudio(chunk) {
					return
				}
				continue
			default:
			}
			select {
			case <-ctx.Done():
				return
			case chunk, ok := <-in:
				if !ok {
					return
				}
				if !realAudio(chunk) {
					return
				}
			case <-deadline:
				deadline = nil
				select {
				case chunk, ok := <-in:
					if !ok {
						return
					}
					if !realAudio(chunk) {
						return
					}
					continue
				default:
				}
				if ctx.Err() == nil && !c.spoken.Load() && c.played.CompareAndSwap(false, true) {
					clip := c.clip()
					if !send(clip) {
						return
					}
					log.Printf("[%s] Initial response silence — playing waiting cue %q (%dms)", h.channelID, clip.text, time.Since(c.started).Milliseconds())
				}
			}
		}
	}()
	return out
}

func toolCueEligible(name string) bool {
	switch name {
	case "", "end_call", "transfer_call", "take_message":
		return false
	default:
		return true
	}
}

// A local tool-only model response closes its speech stream before doing the
// work. Keep the same initial-wait deadline alive during that work. Join any
// already-playing cue before the follow-up, and cancel an unplayed one on return.
func (h *Handler) localWaitCue(ctx context.Context, eligible bool) func() {
	c := h.responseCue
	if !eligible || c == nil || c.spoken.Load() || c.played.Load() || h.rtpReceiver == nil || ctx.Err() != nil {
		return func() {}
	}
	workDone := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		timer := time.NewTimer(c.remaining())
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return
		case <-workDone:
			return
		case <-timer.C:
		}
		select {
		case <-workDone:
			return
		default:
		}
		if ctx.Err() != nil || c.spoken.Load() || !c.played.CompareAndSwap(false, true) {
			return
		}
		clip := c.clip()
		log.Printf("[%s] Initial response silence — playing waiting cue %q during tool work", h.channelID, clip.text)
		if err := h.playWAV(ctx, clip.wav, h.channelID); err != nil && ctx.Err() == nil {
			log.Printf("[%s] Waiting cue playback failed: %v", h.channelID, err)
		}
	}()
	return func() { close(workDone); <-done }
}
