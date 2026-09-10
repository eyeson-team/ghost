package main

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	ghost "github.com/eyeson-team/ghost/v2"
	"github.com/pion/interceptor"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v3"
)

// TestWHIPPublishWithoutOpus covers a sender whose audio cannot be forwarded.
// The publish has to succeed and the video has to keep flowing, only the audio
// section gets rejected.
func TestWHIPPublishWithoutOpus(t *testing.T) {
	video := &countingWriter{}
	audio := &countingWriter{}

	server := NewWHIPServer(WHIPConfig{
		ListenAddr:  "127.0.0.1:18196",
		Path:        "/whip",
		PLIInterval: 0,
		VideoCodecs: mustParseCodecs(t, "h264"),
		Connect: func(VideoCodec) (ghost.RTPWriter, ghost.RTPWriter, error) {
			return video, audio, nil
		},
	})
	if err := server.Start(); err != nil {
		t.Fatalf("failed to start whip server: %v", err)
	}
	waitForListener(t, "http://127.0.0.1:18196/whip")

	// a sender offering H264 video and G.711 audio, no Opus
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
			MimeType:  webrtc.MimeTypePCMU,
			ClockRate: 8000,
		},
		PayloadType: 0,
	}, webrtc.RTPCodecTypeAudio); err != nil {
		t.Fatalf("failed to register pcmu: %v", err)
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
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypePCMU, ClockRate: 8000},
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

	req, _ := http.NewRequest(http.MethodPost, "http://127.0.0.1:18196/whip",
		bytes.NewBufferString(pc.LocalDescription().SDP))
	req.Header.Set("Content-Type", "application/sdp")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("whip post failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected the publish to succeed without opus, got %d", resp.StatusCode)
	}

	answerBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("failed to read answer: %v", err)
	}
	answer := string(answerBytes)

	if !strings.Contains(answer, "m=audio 0 ") {
		t.Errorf("expected the audio section to be rejected, answer was:\n%s", answer)
	}

	if err := pc.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeAnswer,
		SDP:  answer,
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
					SSRC:           9876,
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
		t.Error("no video packets were forwarded")
	}
	if audio.count() != 0 {
		t.Error("no audio should have been forwarded")
	}
}

func TestOfferedAudioCodecs(t *testing.T) {
	offer := "v=0\r\n" +
		"o=- 0 0 IN IP4 127.0.0.1\r\n" +
		"s=-\r\n" +
		"t=0 0\r\n" +
		"m=audio 9 UDP/TLS/RTP/SAVPF 111 0 8\r\n" +
		"c=IN IP4 0.0.0.0\r\n" +
		"a=rtpmap:111 opus/48000/2\r\n" +
		"a=rtpmap:0 PCMU/8000\r\n" +
		"a=rtpmap:8 PCMA/8000\r\n"

	offered, err := OfferedAudioCodecs(offer)
	if err != nil {
		t.Fatalf("failed to parse offer: %v", err)
	}
	if len(offered) != 3 || offered[0] != "OPUS" {
		t.Fatalf("unexpected audio codecs: %v", offered)
	}
	if !OffersOpus(offered) {
		t.Error("expected opus to be detected")
	}
	if OffersOpus([]string{"PCMU", "PCMA"}) {
		t.Error("did not expect opus to be detected")
	}
}

func TestICELinkHeaders(t *testing.T) {
	servers, err := ParseICEServers(
		"stun:stun.eyeson.com:3478,turn:user:pass@turn.eyeson.com:3478?transport=udp")
	if err != nil {
		t.Fatalf("failed to parse ice servers: %v", err)
	}
	settings := ICESettings{Servers: servers, Advertise: true}

	headers := settings.LinkHeaders()
	if len(headers) != 2 {
		t.Fatalf("expected two link headers, got %v", headers)
	}
	if !strings.Contains(headers[0], `<stun:stun.eyeson.com:3478>; rel="ice-server"`) {
		t.Errorf("unexpected stun link header: %s", headers[0])
	}
	if !strings.Contains(headers[1], `username="user"`) ||
		!strings.Contains(headers[1], `credential="pass"`) ||
		!strings.Contains(headers[1], `credential-type="password"`) {
		t.Errorf("unexpected turn link header: %s", headers[1])
	}

	settings.Advertise = false
	if len(settings.LinkHeaders()) != 0 {
		t.Error("expected no link headers when advertising is off")
	}
}

func TestParseICEServers(t *testing.T) {
	servers, err := ParseICEServers(
		"stun:stun.example.com:3478, turn:user:p@ss@turn.example.com:3478?transport=udp")
	if err != nil {
		t.Fatalf("failed to parse: %v", err)
	}
	if len(servers) != 2 {
		t.Fatalf("expected two servers, got %v", servers)
	}
	if servers[0].URL != "stun:stun.example.com:3478" || servers[0].Username != "" {
		t.Errorf("unexpected stun server: %+v", servers[0])
	}
	// credentials are split at the last @, so a password may contain one
	if servers[1].URL != "turn:turn.example.com:3478?transport=udp" ||
		servers[1].Username != "user" || servers[1].Password != "p@ss" {
		t.Errorf("unexpected turn server: %+v", servers[1])
	}

	if _, err := ParseICEServers(""); err != nil {
		t.Errorf("an empty list should be allowed: %v", err)
	}

	for _, invalid := range []string{"stun.example.com:3478", "http://example.com", "turn:"} {
		if _, err := ParseICEServers(invalid); err == nil {
			t.Errorf("expected %q to be rejected", invalid)
		}
	}
}

func TestParseUDPPortRange(t *testing.T) {
	min, max, err := ParseUDPPortRange("50000-50100")
	if err != nil || min != 50000 || max != 50100 {
		t.Fatalf("unexpected result: %d %d %v", min, max, err)
	}

	if _, _, err := ParseUDPPortRange(""); err != nil {
		t.Fatalf("empty range should be allowed: %v", err)
	}

	for _, invalid := range []string{"50000", "50100-50000", "a-b"} {
		if _, _, err := ParseUDPPortRange(invalid); err == nil {
			t.Errorf("expected %q to be rejected", invalid)
		}
	}
}
