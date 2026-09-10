package main

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	ghost "github.com/eyeson-team/ghost/v2"
	"github.com/pion/interceptor"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v3"
)

// countingWriter stands in for the eyeson-side track ghost hands over.
type countingWriter struct {
	mu      sync.Mutex
	packets int
}

func (c *countingWriter) WriteRTP(p *rtp.Packet) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.packets++
	return nil
}

func (c *countingWriter) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.packets
}

// TestWHIPPublish spins up the WHIP endpoint, publishes to it with a pion based
// WHIP client and verifies that the received RTP packets end up on the target
// tracks.
func TestWHIPPublish(t *testing.T) {
	video := &countingWriter{}
	audio := &countingWriter{}

	server := NewWHIPServer(WHIPConfig{
		ListenAddr:  "127.0.0.1:18199",
		Path:        "/whip",
		PLIInterval: 0,
		VideoCodecs: mustParseCodecs(t, "vp9,h264"),
		Connect: func(codec VideoCodec) (ghost.RTPWriter, ghost.RTPWriter, error) {
			if codec.Name != "h264" {
				t.Errorf("expected h264 to be selected, got %s", codec.Name)
			}
			return video, audio, nil
		},
	})
	if err := server.Start(); err != nil {
		t.Fatalf("failed to start whip server: %v", err)
	}
	waitForListener(t, "http://127.0.0.1:18199/whip")

	// a sender that offers H264 and Opus only, like OBS does
	mediaEngine := &webrtc.MediaEngine{}
	if err := mediaEngine.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:  webrtc.MimeTypeH264,
			ClockRate: 90000,
		},
		PayloadType: 96,
	}, webrtc.RTPCodecTypeVideo); err != nil {
		t.Fatalf("failed to register h264: %v", err)
	}
	if err := mediaEngine.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:  webrtc.MimeTypeOpus,
			ClockRate: 48000,
			Channels:  2,
		},
		PayloadType: 111,
	}, webrtc.RTPCodecTypeAudio); err != nil {
		t.Fatalf("failed to register opus: %v", err)
	}
	api := webrtc.NewAPI(webrtc.WithMediaEngine(mediaEngine),
		webrtc.WithInterceptorRegistry(&interceptor.Registry{}))

	pc, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("failed to create sender: %v", err)
	}
	defer pc.Close()

	videoTrack, err := webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264, ClockRate: 90000},
		"video", "whip-test")
	if err != nil {
		t.Fatalf("failed to create video track: %v", err)
	}
	if _, err := pc.AddTrack(videoTrack); err != nil {
		t.Fatalf("failed to add video track: %v", err)
	}

	audioTrack, err := webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2},
		"audio", "whip-test")
	if err != nil {
		t.Fatalf("failed to create audio track: %v", err)
	}
	if _, err := pc.AddTrack(audioTrack); err != nil {
		t.Fatalf("failed to add audio track: %v", err)
	}

	offer, err := pc.CreateOffer(nil)
	if err != nil {
		t.Fatalf("failed to create offer: %v", err)
	}
	gatherComplete := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(offer); err != nil {
		t.Fatalf("failed to set local description: %v", err)
	}
	<-gatherComplete

	req, err := http.NewRequest(http.MethodPost, "http://127.0.0.1:18199/whip",
		bytes.NewBufferString(pc.LocalDescription().SDP))
	if err != nil {
		t.Fatalf("failed to build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/sdp")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("whip post failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 created, got %d", resp.StatusCode)
	}
	location := resp.Header.Get("Location")
	if location == "" {
		t.Fatal("expected a Location header")
	}

	answer, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("failed to read answer: %v", err)
	}
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeAnswer,
		SDP:  string(answer),
	}); err != nil {
		t.Fatalf("failed to set remote description: %v", err)
	}

	// push some packets until they show up on the other side
	go func() {
		sequence := uint16(0)
		timestamp := uint32(0)
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for range ticker.C {
			sequence++
			timestamp += 3000
			packet := &rtp.Packet{
				Header: rtp.Header{
					Version:        2,
					SequenceNumber: sequence,
					Timestamp:      timestamp,
					SSRC:           1234,
				},
				Payload: make([]byte, 200),
			}
			if err := videoTrack.WriteRTP(packet); err != nil {
				return
			}
			if err := audioTrack.WriteRTP(packet); err != nil {
				return
			}
		}
	}()

	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if video.count() > 0 && audio.count() > 0 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	if video.count() == 0 {
		t.Error("no video packets were forwarded")
	}
	if audio.count() == 0 {
		t.Error("no audio packets were forwarded")
	}

	// the sender ends the session
	delReq, err := http.NewRequest(http.MethodDelete, location, nil)
	if err != nil {
		t.Fatalf("failed to build delete request: %v", err)
	}
	delResp, err := http.DefaultClient.Do(delReq)
	if err != nil {
		t.Fatalf("delete failed: %v", err)
	}
	defer delResp.Body.Close()
	if delResp.StatusCode != http.StatusOK {
		t.Errorf("expected 200 on delete, got %d", delResp.StatusCode)
	}
}

// TestWHIPBearerToken verifies the optional token check.
func TestWHIPBearerToken(t *testing.T) {
	server := NewWHIPServer(WHIPConfig{
		ListenAddr:  "127.0.0.1:18198",
		Path:        "/whip",
		BearerToken: "s3cr3t",
		VideoCodecs: mustParseCodecs(t, "h264"),
		Connect: func(VideoCodec) (ghost.RTPWriter, ghost.RTPWriter, error) {
			t.Error("connect must not be called for an unauthorized request")
			return nil, nil, nil
		},
	})
	if err := server.Start(); err != nil {
		t.Fatalf("failed to start whip server: %v", err)
	}
	waitForListener(t, "http://127.0.0.1:18198/whip")

	req, _ := http.NewRequest(http.MethodPost, "http://127.0.0.1:18198/whip",
		bytes.NewBufferString("v=0\r\n"))
	req.Header.Set("Content-Type", "application/sdp")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 without a token, got %d", resp.StatusCode)
	}
}

func waitForListener(t *testing.T, url string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		req, _ := http.NewRequest(http.MethodOptions, url, nil)
		if resp, err := http.DefaultClient.Do(req); err == nil {
			resp.Body.Close()
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("whip endpoint %s did not come up", url)
}

func mustParseCodecs(t *testing.T, list string) []VideoCodec {
	t.Helper()
	codecs, err := ParseVideoCodecs(list)
	if err != nil {
		t.Fatalf("failed to parse codecs %q: %v", list, err)
	}
	return codecs
}

// TestWHIPPublishAnswersBeforeMeetingIsUp is the regression test for a first
// publish that timed out in OBS: the http answer must not wait for the meeting
// connection, and packets must start flowing once it is up.
func TestWHIPPublishSlowConnect(t *testing.T) {
	video := &countingWriter{}
	connectDelay := 3 * time.Second

	server := NewWHIPServer(WHIPConfig{
		ListenAddr:  "127.0.0.1:18195",
		Path:        "/whip",
		PLIInterval: 0,
		VideoCodecs: mustParseCodecs(t, "h264"),
		Connect: func(VideoCodec) (ghost.RTPWriter, ghost.RTPWriter, error) {
			time.Sleep(connectDelay)
			return video, nil, nil
		},
	})
	if err := server.Start(); err != nil {
		t.Fatalf("failed to start whip server: %v", err)
	}
	waitForListener(t, "http://127.0.0.1:18195/whip")

	mediaEngine := &webrtc.MediaEngine{}
	if err := mediaEngine.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:  webrtc.MimeTypeH264,
			ClockRate: 90000,
		},
		PayloadType: 96,
	}, webrtc.RTPCodecTypeVideo); err != nil {
		t.Fatalf("failed to register h264: %v", err)
	}
	api := webrtc.NewAPI(webrtc.WithMediaEngine(mediaEngine),
		webrtc.WithInterceptorRegistry(&interceptor.Registry{}))

	pc, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("failed to create sender: %v", err)
	}
	defer pc.Close()

	videoTrack, err := webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264, ClockRate: 90000},
		"video", "whip-test")
	if err != nil {
		t.Fatalf("failed to create video track: %v", err)
	}
	if _, err := pc.AddTrack(videoTrack); err != nil {
		t.Fatalf("failed to add video track: %v", err)
	}

	offer, err := pc.CreateOffer(nil)
	if err != nil {
		t.Fatalf("failed to create offer: %v", err)
	}
	gatherComplete := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(offer); err != nil {
		t.Fatalf("failed to set local description: %v", err)
	}
	<-gatherComplete

	req, _ := http.NewRequest(http.MethodPost, "http://127.0.0.1:18195/whip",
		bytes.NewBufferString(pc.LocalDescription().SDP))
	req.Header.Set("Content-Type", "application/sdp")

	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("whip post failed: %v", err)
	}
	defer resp.Body.Close()
	elapsed := time.Since(start)

	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 created, got %d", resp.StatusCode)
	}
	if elapsed > connectDelay/2 {
		t.Fatalf("the answer waited %s for the meeting connection, it must not", elapsed)
	}

	answer, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("failed to read answer: %v", err)
	}
	if !strings.Contains(string(answer), "a=candidate:") {
		t.Error("expected the answer to carry ice candidates")
	}
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeAnswer,
		SDP:  string(answer),
	}); err != nil {
		t.Fatalf("failed to set remote description: %v", err)
	}

	go func() {
		sequence := uint16(0)
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for range ticker.C {
			sequence++
			packet := &rtp.Packet{
				Header:  rtp.Header{Version: 2, SequenceNumber: sequence, SSRC: 5555},
				Payload: make([]byte, 200),
			}
			if err := videoTrack.WriteRTP(packet); err != nil {
				return
			}
		}
	}()

	// nothing may be forwarded while the meeting connection is still coming up
	time.Sleep(connectDelay / 2)
	if video.count() != 0 {
		t.Error("packets were forwarded before the meeting connection was ready")
	}

	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if video.count() > 0 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if video.count() == 0 {
		t.Error("no packets were forwarded after the meeting connection came up")
	}
}
