package main

import (
	"testing"
	"time"
)

func TestTimecodeFixerUnwrapsNegativeOffsets(t *testing.T) {
	f := &timecodeFixer{}
	// A cluster at 2.5s containing a block with a relative offset of -1ms,
	// which ebml-go reports as +65535ms.
	in := []time.Duration{
		2400 * time.Millisecond,
		2500 * time.Millisecond,
		2500*time.Millisecond + blockTimecodeWrap - time.Millisecond,
		2600 * time.Millisecond,
	}
	want := []time.Duration{
		2400 * time.Millisecond,
		2500 * time.Millisecond,
		2499 * time.Millisecond,
		2600 * time.Millisecond,
	}
	for i, tc := range in {
		if got := f.fix(tc); got != want[i] {
			t.Errorf("fix(%v) = %v, want %v", tc, got, want[i])
		}
	}
	if f.fixed != 1 {
		t.Errorf("fixed = %d, want 1", f.fixed)
	}
}

func TestTimecodeFixerLeavesNormalStreamsAlone(t *testing.T) {
	f := &timecodeFixer{}
	for i := 0; i < 500; i++ {
		in := time.Duration(i) * 40 * time.Millisecond
		if got := f.fix(in); got != in {
			t.Fatalf("fix(%v) = %v, should be unchanged", in, got)
		}
	}
	if f.fixed != 0 {
		t.Errorf("fixed = %d, want 0", f.fixed)
	}
}

// Reordered timecodes (B-frames) go backwards by small amounts and must not
// be mistaken for a wrap.
func TestTimecodeFixerAllowsReordering(t *testing.T) {
	f := &timecodeFixer{}
	in := []time.Duration{0, 120 * time.Millisecond, 40 * time.Millisecond, 80 * time.Millisecond}
	for _, tc := range in {
		if got := f.fix(tc); got != tc {
			t.Errorf("fix(%v) = %v, should be unchanged", tc, got)
		}
	}
	if f.fixed != 0 {
		t.Errorf("fixed = %d, want 0", f.fixed)
	}
}

// End to end against a real file muxed by ffmpeg with audio, which is what
// triggers the wrapped offsets in practice.
func TestInspectCorrectsWrappedTimecodes(t *testing.T) {
	r, err := inspect("/tmp/media/negtc_audio.webm")
	if err != nil {
		t.Skipf("fixture missing: %v", err)
	}
	if r.fixedTimecodes == 0 {
		t.Skip("fixture has no wrapped timecodes")
	}
	t.Logf("corrected %d wrapped timecodes", r.fixedTimecodes)

	// The file is 60s at 24fps, ~1.4 Mbit/s, keyframes every 2.5s.
	if r.duration < 59*time.Second || r.duration > 61*time.Second {
		t.Errorf("duration = %v, want ~60s", r.duration)
	}
	if r.fps < 23 || r.fps > 25 {
		t.Errorf("fps = %.1f, want ~24", r.fps)
	}
	if r.peakBitrate > 5e6 {
		t.Errorf("peak = %.1f Mbit/s, want ~1.5 (wrap inflates this)", r.peakBitrate/1e6)
	}
	if r.maxKeyframeGap > 5*time.Second {
		t.Errorf("keyframe gap = %v, want ~2.5s", r.maxKeyframeGap)
	}
	if got := r.verdict(); got != 0 {
		t.Errorf("verdict = %d, want 0 (this file is fine)", got)
	}
}
