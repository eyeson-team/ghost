package main

import (
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
)

type mockTrack struct {
	packets []*rtp.Packet
}

func (m *mockTrack) WriteRTP(p *rtp.Packet) error {
	cp := *p
	m.packets = append(m.packets, &cp)
	return nil
}

func tsSpread(pkts []*rtp.Packet) uint32 {
	if len(pkts) == 0 {
		return 0
	}
	min, max := pkts[0].Timestamp, pkts[0].Timestamp
	for _, p := range pkts {
		if p.Timestamp < min {
			min = p.Timestamp
		}
		if p.Timestamp > max {
			max = p.Timestamp
		}
	}
	return max - min
}

func TestProbeAndIngest(t *testing.T) {
	cases := []struct {
		file        string
		wantVideoID string
		wantAudio   bool
		wantErr     bool
	}{
		{"/tmp/media/vp8_opus.webm", codecIDVP8, true, false},
		{"/tmp/media/vp9_opus.webm", codecIDVP9, true, false},
		{"/tmp/media/vp9_vorbis.webm", codecIDVP9, false, false},
		{"/tmp/media/av1_opus.webm", codecIDAV1, true, false},
		{"/tmp/media/h264_opus.mkv", codecIDH264, true, false},
		{"/tmp/media/h264_aac.mkv", codecIDH264, false, false},
		{"/tmp/media/h264_aac.mp4", "", false, true},
	}

	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			plan, err := probe(tc.file, false)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got plan %+v", plan)
				}
				t.Logf("rejected as expected: %v", err)
				return
			}
			if err != nil {
				t.Fatalf("probe failed: %v", err)
			}
			if plan.videoCodecID != tc.wantVideoID {
				t.Errorf("video codec = %q, want %q", plan.videoCodecID, tc.wantVideoID)
			}
			if plan.hasAudio != tc.wantAudio {
				t.Errorf("hasAudio = %v (%s), want %v", plan.hasAudio, plan.audioSkipReason, tc.wantAudio)
			}
			if plan.width != 320 || plan.height != 240 {
				t.Errorf("resolution = %dx%d, want 320x240", plan.width, plan.height)
			}

			vTrack := &mockTrack{}
			aTrack := &mockTrack{}
			vSender := newRtpSender(vTrack, plan.videoPayloader, videoClockRate)
			var aSender *rtpSender
			if plan.hasAudio {
				aSender = newRtpSender(aTrack, &codecs.OpusPayloader{}, opusClockRate)
			}

			start := time.Now()
			dur, err := ingest(tc.file, plan, vSender, aSender, 0)
			if err != nil {
				t.Fatalf("ingest failed: %v", err)
			}
			elapsed := time.Since(start)

			if len(vTrack.packets) == 0 {
				t.Fatal("no video rtp packets produced")
			}
			if plan.hasAudio && len(aTrack.packets) == 0 {
				t.Fatal("no audio rtp packets produced")
			}
			if !plan.hasAudio && len(aTrack.packets) != 0 {
				t.Fatal("audio packets produced although audio is unsupported")
			}

			// The file is 2s long; playback is paced in real time.
			if elapsed < 1500*time.Millisecond || elapsed > 3500*time.Millisecond {
				t.Errorf("playback took %v, want ~2s (real-time pacing broken)", elapsed)
			}

			// Video RTP timestamps must span ~2s at 90kHz (~180000), not
			// 90000 per frame like the original code did.
			vSpread := tsSpread(vTrack.packets)
			if vSpread < 130000 || vSpread > 200000 {
				t.Errorf("video ts spread = %d, want ~180000 for a 2s file", vSpread)
			}
			t.Logf("codec=%s video_pkts=%d audio_pkts=%d duration=%v video_ts_spread=%d",
				plan.videoCodecID, len(vTrack.packets), len(aTrack.packets), dur, vSpread)

			if plan.hasAudio {
				aSpread := tsSpread(aTrack.packets)
				// 2s at 48kHz = ~96000
				if aSpread < 70000 || aSpread > 110000 {
					t.Errorf("audio ts spread = %d, want ~96000", aSpread)
				}
				t.Logf("audio_ts_spread=%d", aSpread)
			}
		})
	}
}

// TestH264AnnexB checks that keyframes carry SPS/PPS and that the AVCC
// length prefixes were replaced by Annex-B start codes.
func TestH264AnnexB(t *testing.T) {
	plan, err := probe("/tmp/media/h264_opus.mkv", false)
	if err != nil {
		t.Fatal(err)
	}
	if plan.videoRewrite == nil {
		t.Fatal("expected a frame rewriter for h264")
	}
	track := &mockTrack{}
	sender := newRtpSender(track, plan.videoPayloader, videoClockRate)
	if _, err := ingest("/tmp/media/h264_opus.mkv", plan, sender, nil, 0); err != nil {
		t.Fatal(err)
	}
	// pion emits STAP-A (type 24) once it has seen SPS+PPS in the stream.
	sawStapA := false
	for _, p := range track.packets {
		if len(p.Payload) > 0 && p.Payload[0]&0x1f == 24 {
			sawStapA = true
			break
		}
	}
	if !sawStapA {
		t.Error("no STAP-A packet found - SPS/PPS were not injected from avcC")
	}
	t.Logf("h264 packets=%d stapA=%v", len(track.packets), sawStapA)
}

// TestLoopTimestampsMonotonic verifies that looping keeps RTP timestamps
// increasing instead of jumping backwards.
func TestLoopTimestampsMonotonic(t *testing.T) {
	plan, err := probe("/tmp/media/vp8_opus.webm", true)
	if err != nil {
		t.Fatal(err)
	}
	track := &mockTrack{}
	sender := newRtpSender(track, plan.videoPayloader, videoClockRate)

	dur, err := ingest("/tmp/media/vp8_opus.webm", plan, sender, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	firstPass := len(track.packets)
	offset := dur + 33*time.Millisecond
	if _, err := ingest("/tmp/media/vp8_opus.webm", plan, sender, nil, offset); err != nil {
		t.Fatal(err)
	}

	last := track.packets[firstPass-1].Timestamp
	next := track.packets[firstPass].Timestamp
	if next <= last {
		t.Errorf("timestamp went backwards across loop: %d -> %d", last, next)
	}
	t.Logf("loop boundary ts %d -> %d (delta %d)", last, next, next-last)
}

type blockingTerminator struct{ called chan struct{} }

func (b *blockingTerminator) TerminateCall() error {
	close(b.called)
	select {} // never returns, like TerminateCall on a dead websocket
}

type okTerminator struct{}

func (okTerminator) TerminateCall() error { return nil }

// TestTerminateCallDoesNotHang covers the shutdown path when the signalling
// websocket is gone and the server never confirms termination.
func TestTerminateCallDoesNotHang(t *testing.T) {
	b := &blockingTerminator{called: make(chan struct{})}
	start := time.Now()
	terminateCall(b)
	elapsed := time.Since(start)

	select {
	case <-b.called:
	default:
		t.Fatal("TerminateCall was never invoked")
	}
	if elapsed < terminateTimeout {
		t.Errorf("returned after %v, expected to wait the full %v", elapsed, terminateTimeout)
	}
	if elapsed > terminateTimeout+2*time.Second {
		t.Errorf("returned after %v, expected to give up at %v", elapsed, terminateTimeout)
	}
	t.Logf("gave up after %v instead of blocking forever", elapsed.Round(time.Millisecond))
}

// A healthy termination must not sit out the timeout.
func TestTerminateCallReturnsPromptly(t *testing.T) {
	start := time.Now()
	terminateCall(okTerminator{})
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("took %v, should return immediately", elapsed)
	}
}
