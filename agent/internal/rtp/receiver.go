// Package rtp provides a UDP RTP receiver for Asterisk External Media (SLIN16).
//
// Asterisk SLIN16: 20ms frames, 16kHz, mono, big-endian int16 PCM.
// Frame size: 320 samples × 2 bytes = 640 bytes payload per packet.
package rtp

import (
	"context"
	"encoding/binary"
	"errors"
	"log"
	"math/rand"
	"net"
	"sync"
	"time"
)

const (
	// RTPHeaderSize is the minimum fixed RTP header size.
	RTPHeaderSize = 12
	// SampleRate is the expected SLIN16 sample rate.
	SampleRate = 16000
	// FrameMS is the frame duration in milliseconds.
	FrameMS = 20
	// FrameSamples is the number of samples per 20ms frame at 16kHz.
	FrameSamples = SampleRate * FrameMS / 1000 // 320
	// FrameBytes is the number of bytes per frame (2 bytes/sample).
	FrameBytes = FrameSamples * 2 // 640

	sampleBufSize = 300 // ~6 seconds of frames

	// payloadTypeOut is the payload type we *send* with: 11, a **static** RTP type.
	// Asterisk streams slin16 to us as dynamic type 118, but it does not map 118 on
	// the *receive* side of an ExternalMedia leg (no SDP exchange ever negotiates a
	// dynamic type there), so mirroring 118 back makes it drop every packet before it
	// becomes an audio frame — the caller hears pure digital silence and nothing is
	// logged. Only static types are in its receive map.
	//
	// A static type also fixes the sample rate: Asterisk reads type 11 as slin at
	// 8kHz, whatever we intend. Sending 16kHz samples under it replays them at half
	// speed (measured: a 440Hz tone comes back at 223Hz — an octave down). So egress
	// is resampled to OutSampleRate. Ingress stays slin16/16kHz for VAD+STT.
	// Measured 2026-07-13 (Asterisk 18.10, two-ExternalMedia loop bench): type 118 →
	// no audio at all; type 11 @16kHz → half pitch; type 11 @8kHz → correct.
	payloadTypeOut = 11

	// OutSampleRate is the rate we send TTS at — pinned by payloadTypeOut. The trunk
	// is 8kHz alaw anyway, so this costs no audible bandwidth.
	OutSampleRate = 8000
	// OutFrameSamples is one 20ms egress frame (160 samples @ 8kHz).
	OutFrameSamples = OutSampleRate * FrameMS / 1000 // 160
)

// Receiver listens on a UDP port for RTP packets and exposes frames
// as []int16 (little-endian, ready for webrtcvad).
type Receiver struct {
	Port int

	conn        *net.UDPConn
	frames      chan []int16
	buf         []byte // inter-packet reassembly buffer
	running     bool
	mu          sync.Mutex
	packetCount int64

	// Outbound (agent TTS → Asterisk) over the *same* ExternalMedia socket.
	// Asterisk's ExternalMedia RTP is bidirectional and symmetric: it streams the
	// caller's audio to us and receives ours back on the address:port it sent from.
	// Because the ExternalMedia channel sits in a *mixing bridge*, audio we send in
	// is mixed to the caller but never reflected back to us — so the agent can no
	// longer hear its own voice (the structural fix for self-interruption). Replaces
	// the old Snoop + ARI /play path.
	remoteAddr *net.UDPAddr // Asterisk's RTP source, learned from the first packet
	outBuf     []int16      // pending TTS samples (16kHz), paced out at one frame/20ms
	outSeq     uint16
	outTS      uint32
	outSSRC    uint32
	firstSent  chan time.Time
	sentCount  int64 // only touched by sendLoop/writeRTP
	wasSilent  bool  // true when the last tick sent nothing → mark next frame (talkspurt)
}

// New creates and binds a UDP receiver on a random port.
func New() (*Receiver, error) {
	addr, err := net.ResolveUDPAddr("udp4", "0.0.0.0:0")
	if err != nil {
		return nil, err
	}
	conn, err := net.ListenUDP("udp4", addr)
	if err != nil {
		return nil, err
	}
	port := conn.LocalAddr().(*net.UDPAddr).Port
	return &Receiver{
		Port:      port,
		conn:      conn,
		frames:    make(chan []int16, sampleBufSize),
		buf:       nil,
		outSSRC:   rand.Uint32(),
		wasSilent: true,
	}, nil
}

// Start launches the receive + send goroutines.
func (r *Receiver) Start() {
	r.mu.Lock()
	r.running = true
	r.mu.Unlock()
	go r.loop()
	go r.sendLoop()
}

// Stop signals the receiver to stop and closes the UDP socket.
func (r *Receiver) Stop() {
	r.mu.Lock()
	r.running = false
	r.mu.Unlock()
	r.conn.Close()
}

// Drain discards all frames currently queued in the buffer.
// Call after playing audio to discard the echo that the mixing bridge
// reflects back before starting the next VAD pass.
func (r *Receiver) Drain() {
	for {
		select {
		case <-r.frames:
		default:
			return
		}
	}
}

// ReadFrame blocks until a complete 20ms frame (320 int16 samples) is available
// or the receiver is stopped. Returns nil on stop/timeout.
func (r *Receiver) ReadFrame(timeout time.Duration) []int16 {
	select {
	case frame := <-r.frames:
		return frame
	case <-time.After(timeout):
		return nil
	}
}

func (r *Receiver) loop() {
	pkt := make([]byte, 4096)
	r.conn.SetDeadline(time.Time{}) // no global deadline

	for {
		r.mu.Lock()
		running := r.running
		r.mu.Unlock()
		if !running {
			return
		}

		r.conn.SetReadDeadline(time.Now().Add(1 * time.Second))
		n, raddr, err := r.conn.ReadFromUDP(pkt)
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			// Socket closed or fatal error
			return
		}

		if n <= RTPHeaderSize {
			continue
		}

		// Learn where to send our TTS back (symmetric RTP). Done once, on the first
		// packet — the send loop stays silent until then. We do *not* reuse Asterisk's
		// payload type for our egress; see payloadTypeOut.
		r.mu.Lock()
		if r.remoteAddr == nil {
			r.remoteAddr = raddr
		}
		r.mu.Unlock()

		r.packetCount++
		if r.packetCount == 1 {
			log.Printf("[rtp port=%d] First RTP packet (%d bytes) from %s pt=%d", r.Port, n, raddr, pkt[1]&0x7f)
		} else if r.packetCount%500 == 0 {
			log.Printf("[rtp port=%d] %d RTP packets received", r.Port, r.packetCount)
		}

		payload := pkt[RTPHeaderSize:n]
		r.buf = append(r.buf, payload...)

		for len(r.buf) >= FrameBytes {
			raw := r.buf[:FrameBytes]
			r.buf = r.buf[FrameBytes:]

			// Byteswap: Asterisk SLIN16 is big-endian, convert to int16 (little-endian host)
			frame := make([]int16, FrameSamples)
			for i := 0; i < FrameSamples; i++ {
				frame[i] = int16(binary.BigEndian.Uint16(raw[i*2:]))
			}

			// Non-blocking put; if full, drop oldest
			select {
			case r.frames <- frame:
			default:
				// Drop oldest
				select {
				case <-r.frames:
				default:
				}
				r.frames <- frame
			}
		}
	}
}

// Enqueue appends TTS samples (int16 at OutSampleRate) to the outbound buffer. They
// are paced out to Asterisk at one 20ms frame per tick by sendLoop. Any length is
// accepted; samples are framed internally.
func (r *Receiver) Enqueue(samples []int16) {
	if len(samples) == 0 {
		return
	}
	r.mu.Lock()
	r.outBuf = append(r.outBuf, samples...)
	r.mu.Unlock()
}

// EnqueueContext caps queued audio at roughly one second. This makes provider
// streaming obey playback backpressure and bounds the memory of long responses.
func (r *Receiver) EnqueueContext(ctx context.Context, pcm []int16) error {
	lastProgress := time.Now()
	for len(pcm) > 0 {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		r.mu.Lock()
		if !r.running {
			r.mu.Unlock()
			return context.Canceled
		}
		room := OutSampleRate - len(r.outBuf)
		if room > 0 {
			if room > len(pcm) {
				room = len(pcm)
			}
			r.outBuf = append(r.outBuf, pcm[:room]...)
			lastProgress = time.Now()
			pcm = pcm[room:]
		}
		r.mu.Unlock()
		if time.Since(lastProgress) > 3*time.Second {
			return errors.New("RTP playback stalled")
		}
		if len(pcm) > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(10 * time.Millisecond):
			}
		}
	}
	return nil
}

// ObserveNextPacket returns the timestamp of the next successful audio send.
// A new turn supersedes an unused observer from a silent or interrupted turn.
func (r *Receiver) ObserveNextPacket() <-chan time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.firstSent = make(chan time.Time, 1)
	return r.firstSent
}

// PendingSamples reports how many outbound samples are still waiting to be sent.
// Callers poll this to block until playback has drained (it reaches 0 once the last
// frame has been written to the socket).
func (r *Receiver) PendingSamples() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.outBuf)
}

// FlushOut discards all pending outbound audio — the barge-in kill switch. The next
// tick sends nothing, so the agent goes silent immediately.
func (r *Receiver) FlushOut() {
	r.mu.Lock()
	r.outBuf = nil
	r.mu.Unlock()
}

// sendLoop paces the outbound buffer to Asterisk: one frame (160 samples, 8kHz,
// big-endian) every 20ms. It sends only when there is audio *and* the remote address
// is known, so silence costs no packets and a real caller barge-in (which flushes
// outBuf) makes the agent go quiet within one frame.
func (r *Receiver) sendLoop() {
	ticker := time.NewTicker(FrameMS * time.Millisecond)
	defer ticker.Stop()

	frame := make([]int16, OutFrameSamples)
	for range ticker.C {
		r.mu.Lock()
		if !r.running {
			r.mu.Unlock()
			return
		}
		if r.remoteAddr == nil || len(r.outBuf) == 0 {
			r.wasSilent = true
			r.mu.Unlock()
			continue
		}
		// Pop one frame; pad the final short frame with silence so the tail is flushed.
		n := OutFrameSamples
		if len(r.outBuf) < OutFrameSamples {
			n = len(r.outBuf)
		}
		copy(frame, r.outBuf[:n])
		for i := n; i < OutFrameSamples; i++ {
			frame[i] = 0
		}
		r.outBuf = r.outBuf[n:]

		addr := r.remoteAddr
		seq, ts, ssrc := r.outSeq, r.outTS, r.outSSRC
		marker := r.wasSilent
		r.wasSilent = false
		r.outSeq++
		r.outTS += OutFrameSamples
		r.mu.Unlock()

		r.writeRTP(addr, payloadTypeOut, marker, seq, ts, ssrc, frame)
	}
}

// writeRTP encodes one RTP packet (12-byte header + big-endian SLIN16 payload) and
// sends it to Asterisk. The marker bit flags the first frame of a talkspurt.
func (r *Receiver) writeRTP(addr *net.UDPAddr, pt byte, marker bool, seq uint16, ts, ssrc uint32, samples []int16) {
	pkt := make([]byte, RTPHeaderSize+len(samples)*2)
	pkt[0] = 0x80 // version 2, no padding/extension/CSRC
	pkt[1] = pt
	if marker {
		pkt[1] |= 0x80
	}
	binary.BigEndian.PutUint16(pkt[2:], seq)
	binary.BigEndian.PutUint32(pkt[4:], ts)
	binary.BigEndian.PutUint32(pkt[8:], ssrc)
	off := RTPHeaderSize
	for _, s := range samples {
		binary.BigEndian.PutUint16(pkt[off:], uint16(s)) // SLIN16 is big-endian on the wire
		off += 2
	}
	if _, err := r.conn.WriteToUDP(pkt, addr); err != nil {
		log.Printf("[rtp port=%d] send error to %s: %v", r.Port, addr, err)
		return
	}
	r.mu.Lock()
	if r.firstSent != nil {
		r.firstSent <- time.Now()
		r.firstSent = nil
	}
	r.mu.Unlock()
	r.sentCount++
	if r.sentCount == 1 {
		log.Printf("[rtp port=%d] First RTP packet SENT (%d bytes) to %s pt=%d", r.Port, len(pkt), addr, pt)
	} else if r.sentCount%250 == 0 {
		log.Printf("[rtp port=%d] %d RTP packets sent", r.Port, r.sentCount)
	}
}
