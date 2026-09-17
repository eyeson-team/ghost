package main

import (
	"bytes"
	"testing"

	"github.com/ebml-go/webm"
	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
)

type payloadTrack struct{ payloads [][]byte }

func (p *payloadTrack) WriteRTP(pkt *rtp.Packet) error {
	p.payloads = append(p.payloads, append([]byte(nil), pkt.Payload...))
	return nil
}

// Every Opus frame in the container must reach the wire byte for byte, as
// exactly one RTP packet. If this holds, any audio problem is on the wire or
// in negotiation, not in how we read and packetize.
func TestOpusPayloadsAreByteExact(t *testing.T) {
	const file = "/tmp/media/negtc_audio.webm"
	plan, err := probe(file, false)
	if err != nil {
		t.Fatal(err)
	}
	tr := &payloadTrack{}
	sender := newRtpSender(tr, &codecs.OpusPayloader{}, opusClockRate)

	_, reader, cleanup, err := openWebM(file)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	var source [][]byte
	fixer := &timecodeFixer{}
	for pkt := range reader.Chan {
		if len(pkt.Data) == 0 {
			break
		}
		if pkt.Timecode == webm.BadTC || pkt.TrackNumber != plan.audioTrackNumber {
			continue
		}
		source = append(source, append([]byte(nil), pkt.Data...))
		sender.send(pkt.Data, fixer.fix(pkt.Timecode))
	}

	if len(tr.payloads) != len(source) {
		t.Fatalf("%d rtp packets for %d opus frames: packetizer is splitting or merging",
			len(tr.payloads), len(source))
	}
	mismatch := 0
	for i := range source {
		if !bytes.Equal(source[i], tr.payloads[i]) {
			mismatch++
		}
	}
	if mismatch > 0 {
		t.Errorf("%d of %d opus payloads altered in transit", mismatch, len(source))
	}
	t.Logf("%d opus frames, all byte-identical on the wire", len(source))
}
