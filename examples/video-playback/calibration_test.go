package main

import (
	"testing"
	"time"
)

// Files that were actually streamed into a meeting, and what happened. The
// verdict must agree with reality: anything observed to stream cleanly has to
// pass, and anything that showed no picture has to fail.
func TestVerdictMatchesObservedResults(t *testing.T) {
	cases := []struct {
		name         string
		avg, p95     float64
		peak         float64
		framePkts    int
		gap          time.Duration
		observed     string
		wantExit     int
		wantNoAdvice bool
	}{
		{
			name: "vp9 720p 1.5Mbit", avg: 1.4e6, p95: 1.5e6, peak: 1.6e6,
			framePkts: 15, gap: 2500 * time.Millisecond,
			observed: "clean", wantExit: 0, wantNoAdvice: true,
		},
		{
			name: "h264 1280x534 3.1Mbit", avg: 2.1e6, p95: 3.1e6, peak: 4.2e6,
			framePkts: 140, gap: 2100 * time.Millisecond,
			observed: "clean, streams seamlessly", wantExit: 0, wantNoAdvice: true,
		},
		{
			name: "vp9 1920x800 3.3Mbit spiky", avg: 2.3e6, p95: 3.3e6, peak: 31.2e6,
			framePkts: 614, gap: 2500 * time.Millisecond,
			observed: "video clean, one heavy second", wantExit: 0,
		},
		{
			name: "vp8 1920x800 11.2Mbit", avg: 6.0e6, p95: 11.2e6, peak: 26.7e6,
			framePkts: 446, gap: 5300 * time.Millisecond,
			observed: "no usable picture", wantExit: 1,
		},
		{
			name: "vp9 broken rc 46Mbit", avg: 31.0e6, p95: 46.1e6, peak: 57.2e6,
			framePkts: 397, gap: 2500 * time.Millisecond,
			observed: "no picture", wantExit: 1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &checkReport{
				path: "x.mkv", plan: &streamPlan{hasAudio: true}, hasAudio: true,
				audioID: codecIDOpus, videoID: codecIDH264,
				width: 1280, fps: 24,
				avgBitrate: tc.avg, p95Bitrate: tc.p95, peakBitrate: tc.peak,
				maxFramePackets: tc.framePkts, maxKeyframeGap: tc.gap,
				keyframes: 100,
			}
			got := r.verdict()
			if got != tc.wantExit {
				t.Errorf("exit = %d, want %d (observed: %s)", got, tc.wantExit, tc.observed)
			}
			// A file that streams cleanly should not be told to re-encode.
			if tc.wantNoAdvice && r.needsAdvice() {
				t.Errorf("advised a re-encode for a file that streams cleanly (%s)", tc.observed)
			}
		})
	}
}
