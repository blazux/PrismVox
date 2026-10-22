package rtp

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestStreamingBackpressureIsBoundedAndCancellable(t *testing.T) {
	r := &Receiver{running: true}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	err := r.EnqueueContext(ctx, make([]int16, OutSampleRate*10))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	if r.PendingSamples() > OutSampleRate {
		t.Fatal("unbounded playback queue")
	}
	r.FlushOut()
	if r.PendingSamples() != 0 {
		t.Fatal("flush failed")
	}
}

func TestStreamingFailsInsteadOfWaitingForeverForMissingRTPPeer(t *testing.T) {
	r := &Receiver{running: true}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := r.EnqueueContext(ctx, make([]int16, OutSampleRate*2))
	if err == nil || !strings.Contains(err.Error(), "stalled") {
		t.Fatalf("missing peer did not report stalled playback: %v", err)
	}
}
