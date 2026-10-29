package call

import (
	"context"
	"encoding/binary"
	"net"
	"testing"
	"time"

	"phoneagent/internal/audio"
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

func TestWaitingCueFinishesBeforeReadyResponseOnRTPAndCallerCanInterrupt(t *testing.T) {
	for _, interrupted := range []bool{false, true} {
		name := "response-waits-for-complete-cue"
		if interrupted {
			name = "caller-interrupts-cue-and-response"
		}
		t.Run(name, func(t *testing.T) {
			receiver, err := rtp.New()
			if err != nil {
				t.Fatal(err)
			}
			receiver.Start()
			defer receiver.Stop()
			peer, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
			if err != nil {
				t.Fatal(err)
			}
			defer peer.Close()
			packet := make([]byte, rtp.RTPHeaderSize+rtp.FrameBytes)
			packet[0], packet[1] = 0x80, 118
			if _, err := peer.WriteToUDP(packet, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: receiver.Port}); err != nil {
				t.Fatal(err)
			}

			// Exactly 1.2 seconds / 60 RTP frames, with a distinct constant level
			// so the receiving end can distinguish cue audio from the response.
			cueSamples := make([]float32, rtp.OutFrameSamples*60)
			for i := range cueSamples {
				cueSamples[i] = 0.25
			}
			wav, err := audio.EncodeWAV(cueSamples, rtp.OutSampleRate)
			if err != nil {
				t.Fatal(err)
			}
			cue := &waitingCue{wav: wav, started: time.Now(), delay: time.Millisecond}
			h := &Handler{channelID: "fixture", rtpReceiver: receiver, responseCue: cue}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			in := make(chan playChunk, 1)
			done := make(chan struct{})
			go func() {
				defer close(done)
				h.playQueued(ctx, h.channelID, h.withWaitCue(ctx, in))
			}()

			readSample := func() int16 {
				t.Helper()
				peer.SetReadDeadline(time.Now().Add(2 * time.Second))
				buf := make([]byte, 2000)
				n, _, err := peer.ReadFromUDP(buf)
				if err != nil || n < rtp.RTPHeaderSize+2 {
					t.Fatalf("missing RTP audio: n=%d err=%v", n, err)
				}
				return int16(binary.BigEndian.Uint16(buf[rtp.RTPHeaderSize:]))
			}
			if sample := readSample(); sample < 4000 || sample > 12000 {
				t.Fatal("first audio was not the waiting cue", sample)
			}
			response := make([]int16, rtp.OutFrameSamples*3)
			for i := range response {
				response[i] = 24000
			}
			// The answer becomes ready immediately after the first cue packet.
			in <- playChunk{pcm: response, text: "Réponse prête."}
			close(in)
			if interrupted {
				cancel()
				select {
				case <-done:
				case <-time.After(time.Second):
					t.Fatal("waiting cue ignored caller interruption")
				}
				if receiver.PendingSamples() != 0 {
					t.Fatal("cue or response remained queued after interruption")
				}
				return
			}

			cueFrames := 1
			for {
				sample := readSample()
				if sample == 24000 {
					break
				}
				if sample < 4000 || sample > 12000 {
					t.Fatal("cue and response audio overlapped", sample)
				}
				cueFrames++
			}
			if cueFrames != 60 {
				t.Fatalf("ready response cut the 1.2s cue short: got %d/60 frames", cueFrames)
			}
			for i := 0; i < 2; i++ {
				if sample := readSample(); sample != 24000 {
					t.Fatal("response did not follow continuously", sample)
				}
			}
			select {
			case <-done:
			case <-ctx.Done():
				t.Fatal("playback did not complete")
			}
		})
	}
}
