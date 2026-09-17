package main

import (
	"bytes"
	"testing"
)

// reassemble turns RFC 7798 packets back into an Annex-B stream, so the
// payloader can be checked against its own inverse.
func reassemble(packets [][]byte) []byte {
	var out []byte
	var fragment []byte
	for _, p := range packets {
		if len(p) < 2 {
			continue
		}
		typ := (p[0] >> 1) & 0x3f
		if typ != h265FragmentationFU {
			out = append(out, annexBStartCode...)
			out = append(out, p...)
			continue
		}
		if len(p) < 3 {
			continue
		}
		fuHeader := p[2]
		if fuHeader&h265FUStartBit != 0 {
			// Rebuild the original two byte NAL header.
			original := (p[0] & 0x81) | ((fuHeader & 0x3f) << 1)
			fragment = []byte{original, p[1]}
		}
		fragment = append(fragment, p[3:]...)
		if fuHeader&h265FUEndBit != 0 {
			out = append(out, annexBStartCode...)
			out = append(out, fragment...)
			fragment = nil
		}
	}
	return out
}

func TestH265PayloaderRoundTrip(t *testing.T) {
	plan, err := probe("/tmp/media/h265.mkv", false)
	if err != nil {
		t.Fatal(err)
	}
	if plan.videoCodecID != codecIDH265 {
		t.Fatalf("codec = %s", plan.videoCodecID)
	}
	if plan.videoRewrite == nil {
		t.Fatal("h265 needs an hvcC rewriter")
	}

	_, reader, cleanup, err := openWebM("/tmp/media/h265.mkv")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	frames, fragmented := 0, 0
	for pkt := range reader.Chan {
		if len(pkt.Data) == 0 {
			break
		}
		if pkt.TrackNumber != plan.videoTrackNumber {
			continue
		}
		annexB := plan.videoRewrite(pkt.Data, pkt.Keyframe)
		packets := plan.videoPayloader.Payload(rtpPayloadBudget, annexB)
		if len(packets) == 0 {
			t.Fatal("no packets produced")
		}
		for _, p := range packets {
			if len(p) > rtpPayloadBudget {
				t.Fatalf("packet of %d bytes exceeds the %d byte budget", len(p), rtpPayloadBudget)
			}
			if (p[0]>>1)&0x3f == h265FragmentationFU {
				fragmented++
			}
		}
		if got := reassemble(packets); !bytes.Equal(got, annexB) {
			t.Fatalf("frame %d did not survive the round trip: %d bytes in, %d out",
				frames, len(annexB), len(got))
		}
		frames++
	}
	if frames == 0 {
		t.Fatal("no frames")
	}
	if fragmented == 0 {
		t.Error("no fragmentation units produced; large NAL units were not exercised")
	}
	t.Logf("%d frames round-tripped, %d fragmentation units", frames, fragmented)
}

// Keyframes must carry VPS, SPS and PPS from hvcC.
func TestH265KeyframeCarriesParameterSets(t *testing.T) {
	plan, err := probe("/tmp/media/h265.mkv", false)
	if err != nil {
		t.Fatal(err)
	}
	_, reader, cleanup, err := openWebM("/tmp/media/h265.mkv")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	for pkt := range reader.Chan {
		if len(pkt.Data) == 0 {
			break
		}
		if pkt.TrackNumber != plan.videoTrackNumber || !pkt.Keyframe {
			continue
		}
		seen := map[uint8]bool{}
		for _, nalu := range splitAnnexB(plan.videoRewrite(pkt.Data, true)) {
			seen[(nalu[0]>>1)&0x3f] = true
		}
		// 32 = VPS, 33 = SPS, 34 = PPS
		for _, want := range []uint8{32, 33, 34} {
			if !seen[want] {
				t.Errorf("keyframe missing NAL type %d", want)
			}
		}
		return
	}
	t.Fatal("no keyframe found")
}

// buildHVCC assembles a minimal hvcC record with the given
// configurationVersion and one VPS, SPS and PPS.
func buildHVCC(version byte) []byte {
	sets := map[byte][]byte{
		32: {0x40, 0x01, 0x0c, 0x01}, // VPS
		33: {0x42, 0x01, 0x01, 0x60}, // SPS
		34: {0x44, 0x01, 0xc0, 0x73}, // PPS
	}
	out := make([]byte, 23)
	out[0] = version
	out[21] = 0xfc | 0x03 // lengthSizeMinusOne = 3, so 4 byte NAL lengths
	out[22] = byte(len(sets))
	for _, typ := range []byte{32, 33, 34} {
		nalu := sets[typ]
		out = append(out, 0x80|typ, 0x00, 0x01) // array header + numNalus = 1
		out = append(out, byte(len(nalu)>>8), byte(len(nalu)))
		out = append(out, nalu...)
	}
	return out
}

// Files muxed while HEVC was still a draft carry configurationVersion 0. They
// are otherwise well formed and must not be rejected.
func TestHVCCAcceptsDraftConfigurationVersion(t *testing.T) {
	for _, version := range []byte{0, 1} {
		rewrite, err := newHVCCToAnnexB(buildHVCC(version))
		if err != nil {
			t.Fatalf("configurationVersion %d rejected: %v", version, err)
		}

		// A keyframe must come back carrying VPS, SPS and PPS.
		frame := []byte{0x00, 0x00, 0x00, 0x02, 0x26, 0x01}
		seen := map[uint8]bool{}
		for _, nalu := range splitAnnexB(rewrite(frame, true)) {
			seen[(nalu[0]>>1)&0x3f] = true
		}
		for _, want := range []uint8{32, 33, 34} {
			if !seen[want] {
				t.Errorf("version %d: keyframe missing NAL type %d", version, want)
			}
		}
	}
}

func TestHVCCRejectsGarbage(t *testing.T) {
	cases := map[string][]byte{
		"too short":         make([]byte, 10),
		"no parameter sets": append(append(make([]byte, 22), 0), make([]byte, 8)...),
	}
	for name, blob := range cases {
		if _, err := newHVCCToAnnexB(blob); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}
