package main

import (
	"os"
	"testing"

	"github.com/ebml-go/webm"
)

// TestH265WireDump writes out exactly what the payloader puts on the wire,
// reassembled back into an Annex-B stream, so that a real decoder can be
// pointed at it. Round-tripping against our own inverse only proves the
// inverse matches; decoding it proves the bitstream is valid.
func TestH265WireDump(t *testing.T) {
	in, out := "/tmp/media/h265.mkv", "/tmp/h265_wire.h265"
	if v := os.Getenv("H265_IN"); v != "" {
		in = v
	}
	if v := os.Getenv("H265_OUT"); v != "" {
		out = v
	}
	plan, err := probe(in, true)
	if err != nil {
		t.Fatal(err)
	}
	_, reader, cleanup, err := openWebM(in)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	f, err := os.Create(out)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	frames, packets := 0, 0
	for pkt := range reader.Chan {
		if len(pkt.Data) == 0 {
			break
		}
		if pkt.TrackNumber != plan.videoTrackNumber || pkt.Timecode == webm.BadTC {
			continue
		}
		annexB := plan.videoRewrite(pkt.Data, pkt.Keyframe)
		wire := plan.videoPayloader.Payload(rtpPayloadBudget, annexB)
		packets += len(wire)
		if _, err := f.Write(reassemble(wire)); err != nil {
			t.Fatal(err)
		}
		frames++
	}
	if frames == 0 {
		t.Fatal("no frames written")
	}
	t.Logf("%s: %d frames -> %d rtp packets, wire bitstream at %s", in, frames, packets, out)
	t.Logf("verify with: ffmpeg -v error -i %s -f null -", out)
}
