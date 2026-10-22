package call

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"phoneagent/internal/rtp"
	"phoneagent/internal/tts"
	"phoneagent/internal/voice"
	"testing"
	"time"
)

func TestCallerReceivesRTPBeforeSynthesisCompletesAndCanInterrupt(t *testing.T) {
	providerStopped := make(chan struct{})
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Text string `json:"text"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body.Text != "Bonjour. Comment puis-je vous aider ?" {
			t.Errorf("short response split before synthesis: %q", body.Text)
		}
		pcm := make([]byte, 3200)
		for i := 0; i < len(pcm); i += 2 {
			binary.LittleEndian.PutUint16(pcm[i:], 1234)
		}
		w.Write(pcm)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(providerStopped)
	}))
	defer provider.Close()
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
	packet[0] = 0x80
	packet[1] = 118
	_, err = peer.WriteToUDP(packet, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: receiver.Port})
	if err != nil {
		t.Fatal(err)
	}
	client := tts.New()
	client.Configure(voice.Settings{TTSProvider: "elevenlabs", BaseURL: provider.URL, VoiceID: "fixture"})
	h := &Handler{channelID: "fixture", rtpReceiver: receiver, ttsClient: client, groupShortSpeech: true}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- h.playText(ctx, "Bonjour. Comment puis-je vous aider ?") }()
	peer.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 2000)
	n, _, err := peer.ReadFromUDP(buf)
	if err != nil {
		t.Fatal("RTP was delayed until full synthesis:", err)
	}
	if n < 14 || buf[1]&0x7f != 11 || binary.BigEndian.Uint16(buf[12:]) != 1234 {
		t.Fatal("wrong RTP audio format")
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("interrupted playback returned success")
		}
	case <-time.After(time.Second):
		t.Fatal("playback did not stop")
	}
	select {
	case <-providerStopped:
	case <-time.After(time.Second):
		t.Fatal("synthesis survived interruption")
	}
	if receiver.PendingSamples() != 0 {
		t.Fatal("cancelled audio remains queued")
	}
}
