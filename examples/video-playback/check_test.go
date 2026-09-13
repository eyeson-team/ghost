package main

import (
	"testing"
	"time"
)

func TestInspect(t *testing.T) {
	cases := []struct {
		file          string
		wantVideo     string
		wantSupported bool
		wantAudioOK   bool
		wantExit      int
	}{
		{"/tmp/media/vp9_opus.webm", "V_VP9", true, true, 0},
		{"/tmp/media/vp9_vorbis.webm", "V_VP9", true, false, 0},
		{"/tmp/media/h264_aac.mkv", "V_MPEG4/ISO/AVC", true, false, 0},
		{"/tmp/media/av1_opus.webm", "V_AV1", true, true, 0},
		{"/tmp/media/h265.mkv", "V_MPEGH/ISO/HEVC", false, false, 1},
		{"/tmp/media/hibitrate_vp9.webm", "V_VP9", true, false, 1},
	}
	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			r, err := inspect(tc.file)
			if err != nil {
				t.Fatalf("inspect: %v", err)
			}
			if r.videoID != tc.wantVideo {
				t.Errorf("video = %q want %q", r.videoID, tc.wantVideo)
			}
			if (r.plan != nil) != tc.wantSupported {
				t.Errorf("supported = %v want %v (%v)", r.plan != nil, tc.wantSupported, r.planErr)
			}
			if r.hasAudio != tc.wantAudioOK {
				t.Errorf("audio = %v want %v", r.hasAudio, tc.wantAudioOK)
			}
			if r.frames == 0 {
				t.Error("no frames counted")
			}
			if r.avgBitrate <= 0 || r.peakBitrate < r.avgBitrate {
				t.Errorf("bitrate avg=%.0f peak=%.0f", r.avgBitrate, r.peakBitrate)
			}
			if got := r.verdict(); got != tc.wantExit {
				t.Errorf("exit = %d want %d", got, tc.wantExit)
			}
		})
	}
}

// The high-bitrate file must be flagged on bitrate alone.
func TestInspectFlagsHighBitrate(t *testing.T) {
	r, err := inspect("/tmp/media/hibitrate_vp9.webm")
	if err != nil {
		t.Fatal(err)
	}
	if r.peakBitrate < 10e6 {
		t.Errorf("peak = %.1f Mbit/s, expected >10", r.peakBitrate/1e6)
	}
	if r.maxFramePackets < 50 {
		t.Errorf("maxFramePackets = %d, expected a large burst", r.maxFramePackets)
	}
	if r.packetsEstimated {
		t.Error("packet count should be exact for a supported codec")
	}
}

// A single keyframe at the start must count the whole file as one gap, not zero.
func TestKeyframeGapIncludesTail(t *testing.T) {
	r, err := inspect("/tmp/media/longgop.webm")
	if err != nil {
		t.Fatal(err)
	}
	if r.keyframes != 1 {
		t.Skipf("expected a single-keyframe fixture, got %d", r.keyframes)
	}
	if r.maxKeyframeGap < 10*time.Second {
		t.Errorf("gap = %v, expected the full file length", r.maxKeyframeGap)
	}
}

// Short clips with one keyframe are normal and must not warn.
func TestShortClipNotFlagged(t *testing.T) {
	r, err := inspect("/tmp/media/vp9_opus.webm")
	if err != nil {
		t.Fatal(err)
	}
	if r.maxKeyframeGap > longKeyframeGap {
		t.Errorf("2s clip flagged with gap %v", r.maxKeyframeGap)
	}
}

func TestInspectRejectsMP4(t *testing.T) {
	if _, err := inspect("/tmp/media/h264_aac.mp4"); err == nil {
		t.Fatal("expected mp4 to be rejected")
	}
}

func TestSuggestedCommandCopiesWhenPossible(t *testing.T) {
	r, err := inspect("/tmp/media/h264_aac.mkv")
	if err != nil {
		t.Fatal(err)
	}
	cmd := suggestedCommand(r, false)
	if want := "-c:v copy"; !contains(cmd, want) {
		t.Errorf("cmd = %q, should keep video as-is via %q", cmd, want)
	}
	if want := "-c:a libopus"; !contains(cmd, want) {
		t.Errorf("cmd = %q, should convert audio via %q", cmd, want)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
