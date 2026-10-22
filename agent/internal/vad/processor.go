// Package vad provides speech detection using the WebRTC VAD library.
//
// Algorithm mirrors the Python vad.py implementation:
//   - 20ms frames at 16kHz (320 samples)
//   - 15-frame (300ms) ring buffer for decision window
//   - Speech triggered when ≥90% of ring buffer frames are voiced
//   - Speech ends when ≥90% of ring buffer frames are unvoiced
//   - Minimum utterance: 8 frames (160ms)
package vad

import (
	"context"
	"log"
	"time"

	webrtcvad "github.com/maxhawkins/go-webrtcvad"

	"phoneagent/internal/rtp"
)

const (
	// triggerRatio is the voiced fraction required to start capture.
	triggerRatio = 0.90
	// endRatio is the unvoiced fraction required to end capture.
	endRatio = 0.90
	// ringFrames is the size of the decision ring buffer (15 × 20ms = 300ms).
	ringFrames = 15
	// minSpeechFrames is the minimum utterance length before returning (8 × 20ms = 160ms).
	minSpeechFrames = 8
	// frameReadTimeout is how long ReadFrame blocks before giving up (allows detecting stop).
	frameReadTimeout = 5 * time.Second
	// preRollFrames is how much audio preceding the VAD trigger is kept and
	// prepended to the capture (25 × 20ms = 500ms). Triggering needs a 300ms
	// mostly-voiced window, so without pre-roll the first words are lost —
	// "vas-y passe-le-moi" reached Whisper as "passe le mois" and a transfer
	// acceptance was classified as a refusal.
	preRollFrames = 25
)

// appendPreRoll appends frame to the pre-trigger rolling buffer, capped at
// preRollFrames.
func appendPreRoll(pre [][]int16, frame []int16) [][]int16 {
	pre = append(pre, frame)
	if len(pre) > preRollFrames {
		pre = pre[len(pre)-preRollFrames:]
	}
	return pre
}

// FrameSource yields 20ms frames of PCM (nil on timeout/stop). *rtp.Receiver is
// the production implementation; the seam lets the capture logic be tested
// without a UDP socket.
type FrameSource interface {
	ReadFrame(timeout time.Duration) []int16
}

// Processor detects speech on an RTP stream using WebRTC VAD.
type Processor struct {
	vad *webrtcvad.VAD
}

// New creates a VAD processor with aggressiveness level 2 (normal capture).
func New() (*Processor, error) {
	return NewWithMode(2)
}

// NewWithMode creates a VAD processor with a given aggressiveness (0–3). Higher is
// stricter at rejecting non-speech — useful for barge-in in noisy environments,
// where the lenient mode 2 lets background noise read as voiced.
func NewWithMode(mode int) (*Processor, error) {
	v, err := webrtcvad.New()
	if err != nil {
		return nil, err
	}
	if err := v.SetMode(mode); err != nil {
		return nil, err
	}
	return &Processor{vad: v}, nil
}

// GetSpeechSegment blocks until a complete utterance is detected on r.
// Returns the accumulated int16 samples (big-endian swapped, i.e. actual LE int16),
// or nil if the receiver is stopped.
func (p *Processor) GetSpeechSegment(r FrameSource) []int16 {
	seg, _ := p.GetSpeechSegmentTimeout(r, 0)
	return seg
}

// GetSpeechSegmentTimeout is GetSpeechSegment with a silence deadline: if no
// speech has *started* within maxWait, it returns (nil, true). Once speech is
// triggered the utterance is always captured to completion. maxWait <= 0
// disables the deadline.
func (p *Processor) GetSpeechSegmentTimeout(r FrameSource, maxWait time.Duration) ([]int16, bool) {
	return p.GetSpeechSegmentTimeoutContext(context.Background(), r, maxWait)
}

// GetSpeechSegmentTimeoutContext also stops an utterance when the call ends.
func (p *Processor) GetSpeechSegmentTimeoutContext(ctx context.Context, r FrameSource, maxWait time.Duration) ([]int16, bool) {
	return p.Capture(ctx, r, maxWait, nil)
}

// Capture optionally delivers the same utterance frames during detection, including
// pre-roll. The callback must not block the RTP reader.
func (p *Processor) Capture(ctx context.Context, r FrameSource, maxWait time.Duration, onAudio func([]int16)) ([]int16, bool) {
	var (
		ring         = make([]bool, 0, ringFrames)
		pre          [][]int16 // rolling pre-trigger audio, prepended at onset
		speechFrames [][]int16
		triggered    bool
		start        = time.Now()
		lastVoiced   time.Time // for eot_wait: trailing silence before endpoint fires
	)

	for {
		if ctx.Err() != nil {
			return nil, false
		}
		if maxWait > 0 && !triggered && time.Since(start) > maxWait {
			return nil, true
		}
		frame := r.ReadFrame(frameReadTimeout)
		if frame == nil {
			return nil, false
		}

		// webrtcvad expects raw bytes of int16 LE PCM
		frameBytes := int16SliceToBytes(frame)
		isSpeech, err := p.vad.Process(rtp.SampleRate, frameBytes)
		if err != nil {
			continue
		}

		if !triggered {
			pre = appendPreRoll(pre, frame)
			ring = appendRing(ring, isSpeech, ringFrames)
			if len(ring) == ringFrames && voicedFraction(ring) >= triggerRatio {
				triggered = true
				speechFrames = append([][]int16{}, pre...) // pre includes this frame
				if onAudio != nil {
					for _, f := range pre {
						onAudio(f)
					}
				}
				pre = nil
				lastVoiced = time.Now()
				ring = ring[:0]
				log.Println("VAD: speech start")
			}
		} else {
			speechFrames = append(speechFrames, frame)
			if onAudio != nil {
				onAudio(frame)
			}
			if isSpeech {
				lastVoiced = time.Now()
			}
			ring = appendRing(ring, isSpeech, ringFrames)

			if len(ring) == ringFrames {
				unvoicedFrac := 1.0 - voicedFraction(ring)
				if unvoicedFrac >= endRatio {
					log.Printf("VAD: speech end (%d frames, eot=%dms)",
						len(speechFrames), time.Since(lastVoiced).Milliseconds())

					if len(speechFrames) < minSpeechFrames {
						speechFrames = speechFrames[:0]
						ring = ring[:0]
						triggered = false
						continue
					}

					// Flatten all frames into one slice
					out := make([]int16, 0, len(speechFrames)*rtp.FrameSamples)
					for _, f := range speechFrames {
						out = append(out, f...)
					}
					return out, false
				}
			}
		}
	}
}

// GetSpeechSegmentCtx captures one utterance like GetSpeechSegmentTimeout, but for
// barge-in monitoring during playback:
//   - it aborts (returns nil) if ctx is cancelled *before* speech onset;
//   - onset uses the same reliable 300ms/90% window as normal capture, but the
//     utterance is only *considered* once sustainFrames of voiced speech have
//     accrued, so brief backchannels ("oui", "d'accord") never cut the agent off;
//   - at every additional sustainFrames of voiced audio, onSustained is called
//     (synchronously) with the samples captured so far. It decides whether this
//     is a genuine interruption (true — e.g. after validating via STT that it is
//     not the agent's own playback echoing back) or not (false — keep watching,
//     re-check after the next sustainFrames). This matters on echo-prone trunks:
//     echo keeps the stream voiced for as long as the agent speaks, so "wait for
//     the utterance to end, then decide" would never decide;
//   - an utterance that ends before being confirmed is discarded and monitoring
//     continues (no interrupt, not consumed as a turn).
//
// Once confirmed the utterance is captured to completion, ignoring ctx.
func (p *Processor) GetSpeechSegmentCtx(ctx context.Context, r FrameSource, sustainFrames int, onSustained func(partial []int16) bool) []int16 {
	return p.GetSpeechSegmentCallCtx(context.Background(), ctx, r, sustainFrames, onSustained)
}

// GetSpeechSegmentCallCtx finishes a confirmed interruption after playback stops,
// but always abandons capture when the telephone call itself has ended.
func (p *Processor) GetSpeechSegmentCallCtx(callCtx, ctx context.Context, r FrameSource, sustainFrames int, onSustained func([]int16) bool) []int16 {
	if sustainFrames < minSpeechFrames {
		sustainFrames = minSpeechFrames
	}
	var (
		ring         = make([]bool, 0, ringFrames)
		pre          [][]int16 // rolling pre-trigger audio, prepended at onset
		speechFrames [][]int16
		voicedCount  int
		nextCheck    int
		triggered    bool
		fired        bool
	)

	flatten := func() []int16 {
		out := make([]int16, 0, len(speechFrames)*rtp.FrameSamples)
		for _, f := range speechFrames {
			out = append(out, f...)
		}
		return out
	}

	for {
		if callCtx.Err() != nil {
			return nil
		}
		if !fired && ctx.Err() != nil {
			return nil
		}
		// Short read timeout so cancellation is observed within ~200ms.
		frame := r.ReadFrame(200 * time.Millisecond)
		if frame == nil {
			if ctx.Err() != nil {
				// RTP stopped and the call is tearing down: deliver what a
				// confirmed capture has, abandon anything else — never spin
				// forever on a dead stream.
				if fired {
					return flatten()
				}
				return nil
			}
			continue
		}
		isSpeech, err := p.vad.Process(rtp.SampleRate, int16SliceToBytes(frame))
		if err != nil {
			continue
		}

		if !triggered {
			pre = appendPreRoll(pre, frame)
			ring = appendRing(ring, isSpeech, ringFrames)
			if len(ring) == ringFrames && voicedFraction(ring) >= triggerRatio {
				triggered = true
				speechFrames = append([][]int16{}, pre...) // pre includes this frame
				pre = nil
				voicedCount = ringFrames // the onset window was voiced
				nextCheck = sustainFrames
				ring = ring[:0]
			}
		} else {
			speechFrames = append(speechFrames, frame)
			if isSpeech {
				voicedCount++
			}
			// Enough voiced speech accrued — let the caller-supplied validator
			// decide (frames buffer in the receiver while it runs, ~6s headroom).
			if !fired && voicedCount >= nextCheck {
				log.Printf("VAD: barge-in candidate (%d voiced) — validating", voicedCount)
				if onSustained == nil || onSustained(flatten()) {
					fired = true
				} else {
					nextCheck = voicedCount + sustainFrames
				}
			}
			ring = appendRing(ring, isSpeech, ringFrames)
			if len(ring) == ringFrames && 1.0-voicedFraction(ring) >= endRatio {
				if !fired {
					// Ended before being confirmed → backchannel/echo/noise; ignore,
					// keep watching.
					log.Printf("VAD: ignored unconfirmed utterance (%d voiced)", voicedCount)
					speechFrames = speechFrames[:0]
					voicedCount = 0
					ring = ring[:0]
					triggered = false
					continue
				}
				log.Printf("VAD: speech end (%d frames)", len(speechFrames))
				return flatten()
			}
		}
	}
}

// appendRing appends v to the ring slice, keeping at most maxLen elements.
func appendRing(ring []bool, v bool, maxLen int) []bool {
	ring = append(ring, v)
	if len(ring) > maxLen {
		ring = ring[len(ring)-maxLen:]
	}
	return ring
}

// voicedFraction returns the fraction of true values in ring.
func voicedFraction(ring []bool) float64 {
	if len(ring) == 0 {
		return 0
	}
	count := 0
	for _, v := range ring {
		if v {
			count++
		}
	}
	return float64(count) / float64(len(ring))
}

// int16SliceToBytes converts []int16 to []byte (little-endian).
func int16SliceToBytes(samples []int16) []byte {
	b := make([]byte, len(samples)*2)
	for i, s := range samples {
		b[i*2] = byte(s)
		b[i*2+1] = byte(s >> 8)
	}
	return b
}
