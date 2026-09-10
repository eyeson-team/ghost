package main

import (
	"bytes"
	"io"
	"net/http"
	"testing"
	"time"

	ghost "github.com/eyeson-team/ghost/v2"
	"github.com/pion/interceptor"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v3"
)

func TestOfferedVideoCodecs(t *testing.T) {
	offer := "v=0\r\n" +
		"o=- 0 0 IN IP4 127.0.0.1\r\n" +
		"s=-\r\n" +
		"t=0 0\r\n" +
		"m=audio 9 UDP/TLS/RTP/SAVPF 111\r\n" +
		"c=IN IP4 0.0.0.0\r\n" +
		"a=rtpmap:111 opus/48000/2\r\n" +
		"m=video 9 UDP/TLS/RTP/SAVPF 96 97 98 99\r\n" +
		"c=IN IP4 0.0.0.0\r\n" +
		"a=rtpmap:96 VP8/90000\r\n" +
		"a=rtpmap:97 rtx/90000\r\n" +
		"a=rtpmap:98 VP9/90000\r\n" +
		"a=rtpmap:99 AV1/90000\r\n"

	offered, err := OfferedVideoCodecs(offer)
	if err != nil {
		t.Fatalf("failed to parse offer: %v", err)
	}

	expected := []string{webrtc.MimeTypeVP8, webrtc.MimeTypeVP9, webrtc.MimeTypeAV1}
	if len(offered) != len(expected) {
		t.Fatalf("expected %v, got %v", expected, offered)
	}
	for i := range expected {
		if offered[i] != expected[i] {
			t.Fatalf("expected %v, got %v", expected, offered)
		}
	}
}

func TestSelectVideoCodec(t *testing.T) {
	preference, err := ParseVideoCodecs("vp9,av1,vp8,h264")
	if err != nil {
		t.Fatalf("failed to parse preference: %v", err)
	}

	cases := []struct {
		name     string
		offered  []string
		expected string
		ok       bool
	}{
		{"server preference wins", []string{webrtc.MimeTypeH264, webrtc.MimeTypeVP9}, "vp9", true},
		{"h264 only sender", []string{webrtc.MimeTypeH264}, "h264", true},
		{"av1 beats vp8", []string{webrtc.MimeTypeVP8, webrtc.MimeTypeAV1}, "av1", true},
		{"nothing in common", []string{webrtc.MimeTypeH265}, "", false},
		{"audio only sender", []string{}, "", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			codec, ok := SelectVideoCodec(preference, tc.offered)
			if ok != tc.ok {
				t.Fatalf("expected ok=%v, got %v", tc.ok, ok)
			}
			if ok && codec.Name != tc.expected {
				t.Fatalf("expected %s, got %s", tc.expected, codec.Name)
			}
		})
	}
}

func TestParseVideoCodecsRejectsUnknown(t *testing.T) {
	if _, err := ParseVideoCodecs("vp9,theora"); err == nil {
		t.Fatal("expected an error for an unknown codec")
	}
}

// TestWHIPPublishVP9 publishes with a sender that only offers VP9 and checks
// that the endpoint picks VP9 and forwards the packets.
func TestWHIPPublishVP9(t *testing.T) {
	video := &countingWriter{}

	var selected VideoCodec
	server := NewWHIPServer(WHIPConfig{
		ListenAddr:  "127.0.0.1:18197",
		Path:        "/whip",
		PLIInterval: 0,
		VideoCodecs: mustParseCodecs(t, "vp9,av1,vp8,h264"),
		Connect: func(codec VideoCodec) (ghost.RTPWriter, ghost.RTPWriter, error) {
			selected = codec
			return video, nil, nil
		},
	})
	if err := server.Start(); err != nil {
		t.Fatalf("failed to start whip server: %v", err)
	}
	waitForListener(t, "http://127.0.0.1:18197/whip")

	// a sender that can only do VP9
	mediaEngine := &webrtc.MediaEngine{}
	if err := mediaEngine.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:  webrtc.MimeTypeVP9,
			ClockRate: 90000,
		},
		PayloadType: 98,
	}, webrtc.RTPCodecTypeVideo); err != nil {
		t.Fatalf("failed to register vp9: %v", err)
	}
	api := webrtc.NewAPI(webrtc.WithMediaEngine(mediaEngine),
		webrtc.WithInterceptorRegistry(&interceptor.Registry{}))

	pc, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("failed to create sender: %v", err)
	}
	defer pc.Close()

	videoTrack, err := webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP9, ClockRate: 90000},
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

	req, _ := http.NewRequest(http.MethodPost, "http://127.0.0.1:18197/whip",
		bytes.NewBufferString(pc.LocalDescription().SDP))
	req.Header.Set("Content-Type", "application/sdp")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("whip post failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 created, got %d", resp.StatusCode)
	}

	if selected.Name != "vp9" {
		t.Fatalf("expected vp9 to be selected, got %q", selected.Name)
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
					SSRC:           4321,
				},
				Payload: make([]byte, 200),
			}
			if err := videoTrack.WriteRTP(packet); err != nil {
				return
			}
		}
	}()

	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if video.count() > 0 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	if video.count() == 0 {
		t.Error("no vp9 packets were forwarded")
	}
}
