package main

import (
	"testing"
	"time"
)

func TestBitrateStatsIgnoresSingleOutlier(t *testing.T) {
	buckets := map[int64]int64{}
	for i := int64(0); i < 100; i++ {
		buckets[i] = 250_000 // 2 Mbit/s
	}
	buckets[50] = 3_900_000 // one 31 Mbit/s second
	peak, p95 := bitrateStats(buckets)
	if peak < 30e6 {
		t.Errorf("peak = %.1f Mbit/s, want ~31", peak/1e6)
	}
	if p95 > 3e6 {
		t.Errorf("p95 = %.1f Mbit/s, want ~2 (outlier must not dominate)", p95/1e6)
	}
}

// Mirrors tears_of_steel_vp9.webm: fine on average, one huge second.
func TestSpikyFilmIsNotRejected(t *testing.T) {
	r := &checkReport{
		plan:            &streamPlan{hasAudio: true},
		hasAudio:        true,
		audioID:         codecIDOpus,
		videoID:         codecIDVP9,
		avgBitrate:      2.3e6,
		p95Bitrate:      4.1e6,
		peakBitrate:     31.2e6,
		maxFrameBytes:   710 * 1024,
		maxFramePackets: 614,
		keyframes:       294,
		maxKeyframeGap:  2500 * time.Millisecond,
	}
	if got := r.verdict(); got != 0 {
		t.Errorf("exit = %d, want 0: a 2.3 Mbit/s film should not be rejected", got)
	}
}

// Sustained high bitrate must still be rejected.
func TestSustainedHighBitrateStillRejected(t *testing.T) {
	r := &checkReport{
		plan:        &streamPlan{},
		videoID:     codecIDVP9,
		avgBitrate:  14.8e6,
		p95Bitrate:  16.0e6,
		peakBitrate: 16.5e6,
	}
	if got := r.verdict(); got != 1 {
		t.Errorf("exit = %d, want 1", got)
	}
}

// The suggestion for a spiky 1080p-wide film must reach for resolution, not
// repeat the bitrate flags the user has already applied.
func TestSuggestionUsesResolutionLever(t *testing.T) {
	r := &checkReport{
		path: "tears_of_steel_vp9_2.webm", plan: &streamPlan{hasAudio: true},
		hasAudio: true, audioID: codecIDOpus, videoID: codecIDVP9,
		width: 1920, height: 800, fps: 24,
		avgBitrate: 2.3e6, p95Bitrate: 3.3e6, peakBitrate: 31.2e6,
		maxKeyframeGap: 2500 * time.Millisecond,
	}
	cmd := suggestedCommand(r, false)
	t.Log(cmd)
	for _, want := range []string{"scale=1280:-2", "libx264", "out.mkv", "-c:a copy"} {
		if !contains(cmd, want) {
			t.Errorf("cmd missing %q:\n  %s", want, cmd)
		}
	}
	if contains(cmd, "-r 25") {
		t.Error("24 fps must not get a frame rate override")
	}
}

// Above 25 fps the server cannot use the extra frames.
func TestSuggestionCapsFrameRate(t *testing.T) {
	r := &checkReport{
		path: "x.webm", plan: &streamPlan{}, videoID: codecIDVP9,
		width: 1280, fps: 60,
		avgBitrate: 1e6, p95Bitrate: 1.2e6, peakBitrate: 1.3e6,
	}
	if cmd := suggestedCommand(r, false); !contains(cmd, "-r 25") {
		t.Errorf("cmd missing frame rate cap:\n  %s", cmd)
	}
	if got := r.verdict(); got != 0 {
		t.Errorf("exit = %d, want 0", got)
	}
}

// A file that only needs its audio converted must keep the video untouched.
func TestSuggestionKeepsGoodVideo(t *testing.T) {
	r := &checkReport{
		path: "x.webm", plan: &streamPlan{}, videoID: codecIDVP9, audioID: codecIDVorbis,
		width: 1280, fps: 25,
		avgBitrate: 1e6, p95Bitrate: 1.2e6, peakBitrate: 1.3e6,
		maxKeyframeGap: time.Second,
	}
	cmd := suggestedCommand(r, false)
	if !contains(cmd, "-c:v copy") || !contains(cmd, "-c:a libopus") {
		t.Errorf("want video copied and audio converted:\n  %s", cmd)
	}
}
