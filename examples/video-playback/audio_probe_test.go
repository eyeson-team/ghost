package main

import (
	"sort"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
)

type stampedTrack struct {
	at []time.Duration
	ts []uint32
	t0 time.Time
}

func (s *stampedTrack) WriteRTP(p *rtp.Packet) error {
	s.at = append(s.at, time.Since(s.t0))
	s.ts = append(s.ts, p.Timestamp)
	return nil
}

func TestAudioNotDelayedByVideo(t *testing.T) {
	plan, err := probe("/tmp/media/bigframes.webm", false)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.hasAudio {
		t.Fatal("fixture should have opus audio")
	}
	now := time.Now()
	vt := &stampedTrack{t0: now}
	at := &stampedTrack{t0: now}
	vs := newRtpSender(vt, plan.videoPayloader, videoClockRate)
	as := newRtpSender(at, &codecs.OpusPayloader{}, opusClockRate)

	if _, err := ingest("/tmp/media/bigframes.webm", plan, vs, as, 0); err != nil {
		t.Fatal(err)
	}

	// RTP timestamp deltas: opus at 20ms and 48kHz = 960 samples apart
	tsBad := 0
	for i := 1; i < len(at.ts); i++ {
		if d := int64(at.ts[i]) - int64(at.ts[i-1]); d != 960 {
			tsBad++
			if tsBad <= 3 {
				t.Logf("  ts delta at %d = %d (want 960)", i, d)
			}
		}
	}
	t.Logf("audio packets=%d, non-960 ts deltas=%d", len(at.ts), tsBad)

	// Wall clock spacing: how late does each audio packet actually go out?
	var gaps []float64
	for i := 1; i < len(at.at); i++ {
		gaps = append(gaps, float64(at.at[i]-at.at[i-1])/float64(time.Millisecond))
	}
	sort.Float64s(gaps)
	p50 := gaps[len(gaps)/2]
	p95 := gaps[int(0.95*float64(len(gaps)))]
	t.Logf("audio send gaps ms: median=%.1f p95=%.1f max=%.1f (want ~20)",
		p50, p95, gaps[len(gaps)-1])

	// Audio must not be delayed behind video pacing. Before the reader ran
	// ahead of both tracks, a file with large frames pushed p95 to ~38ms and
	// the maximum to ~58ms, which is audible as scratchy audio.
	if p95 > 25 {
		t.Errorf("audio p95 gap = %.1fms, want ~20: audio is stuck behind video", p95)
	}
	// The single worst gap is at the mercy of the scheduler, so measure how
	// often audio runs late instead. Before the reader ran ahead, well over a
	// twentieth of all packets exceeded this.
	late := 0
	for _, g := range gaps {
		if g > 30 {
			late++
		}
	}
	if ratio := float64(late) / float64(len(gaps)); ratio > 0.01 {
		t.Errorf("%.1f%% of audio packets sent more than 30ms apart (%d of %d), want under 1%%",
			ratio*100, late, len(gaps))
	}
	if tsBad > 1 {
		t.Errorf("%d irregular rtp timestamp deltas", tsBad)
	}
	t.Logf("video packets=%d", len(vt.ts))
}
