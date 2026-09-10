package main

import (
	"fmt"
	"strings"

	ghost "github.com/eyeson-team/ghost/v2"
	"github.com/pion/sdp/v3"
	"github.com/pion/webrtc/v3"
)

// VideoCodec ties together the three names one codec has in this example: the
// name used on the command line, the mime type used by pion and in SDP, and the
// ghost option that makes the eyeson side negotiate it.
type VideoCodec struct {
	Name        string
	MimeType    string
	GhostOption ghost.ClientOption
}

// DefaultVideoCodecs is the preference order used when nothing else is given.
// The eyeson media server prefers VP9, so that is tried first, and H264 is kept
// last as the codec every WHIP sender is guaranteed to have.
const DefaultVideoCodecs = "vp9,av1,vp8,h264"

var knownVideoCodecs = []VideoCodec{
	{Name: "vp9", MimeType: webrtc.MimeTypeVP9, GhostOption: ghost.WithForceVP9Codec()},
	{Name: "av1", MimeType: webrtc.MimeTypeAV1, GhostOption: ghost.WithForceAV1Codec()},
	{Name: "vp8", MimeType: webrtc.MimeTypeVP8, GhostOption: ghost.WithForceVP8Codec()},
	{Name: "h264", MimeType: webrtc.MimeTypeH264, GhostOption: ghost.WithForceH264Codec()},
	// H265 is listed because both OBS and ghost can do it. Whether the meeting
	// server accepts it is a different question, which is why it is not part of
	// DefaultVideoCodecs.
	{Name: "h265", MimeType: webrtc.MimeTypeH265, GhostOption: ghost.WithForceH265Codec()},
}

// sdpNameToMimeType maps the encoding names that show up in a=rtpmap lines onto
// pion mime types. AV1X is what older Chrome versions offered, HEVC is what some
// senders use instead of H265.
var sdpNameToMimeType = map[string]string{
	"H264": webrtc.MimeTypeH264,
	"VP8":  webrtc.MimeTypeVP8,
	"VP9":  webrtc.MimeTypeVP9,
	"AV1":  webrtc.MimeTypeAV1,
	"AV1X": webrtc.MimeTypeAV1,
	"H265": webrtc.MimeTypeH265,
	"HEVC": webrtc.MimeTypeH265,
}

// ParseVideoCodecs turns a comma separated list like "vp9,h264" into the codec
// preference order used when answering a WHIP offer.
func ParseVideoCodecs(list string) ([]VideoCodec, error) {
	result := []VideoCodec{}
	for _, entry := range strings.Split(list, ",") {
		name := strings.ToLower(strings.TrimSpace(entry))
		if name == "" {
			continue
		}
		found := false
		for _, codec := range knownVideoCodecs {
			if codec.Name == name {
				result = append(result, codec)
				found = true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("unknown video codec %q, known are %s",
				name, knownVideoCodecNames())
		}
	}
	if len(result) == 0 {
		return nil, fmt.Errorf("no video codec configured")
	}
	return result, nil
}

func knownVideoCodecNames() string {
	names := make([]string, 0, len(knownVideoCodecs))
	for _, codec := range knownVideoCodecs {
		names = append(names, codec.Name)
	}
	return strings.Join(names, ", ")
}

// encodingNames returns the rtpmap encoding names of one media kind ("video" or
// "audio") in an SDP offer, in the order the sender listed them.
func encodingNames(offer, kind string) ([]string, error) {
	parsed := sdp.SessionDescription{}
	if err := parsed.Unmarshal([]byte(offer)); err != nil {
		return nil, err
	}

	names := []string{}
	seen := map[string]bool{}

	for _, media := range parsed.MediaDescriptions {
		if media.MediaName.Media != kind {
			continue
		}
		for _, attribute := range media.Attributes {
			if attribute.Key != "rtpmap" {
				continue
			}
			// a=rtpmap:96 VP8/90000
			fields := strings.Fields(attribute.Value)
			if len(fields) < 2 {
				continue
			}
			name := strings.ToUpper(strings.Split(fields[1], "/")[0])
			if seen[name] {
				continue
			}
			seen[name] = true
			names = append(names, name)
		}
	}

	return names, nil
}

// OfferedVideoCodecs returns the mime types of all video codecs found in an SDP
// offer, in the order the sender listed them. Unknown encodings and the helper
// formats (rtx, red, ulpfec, flexfec) are left out.
func OfferedVideoCodecs(offer string) ([]string, error) {
	names, err := encodingNames(offer, "video")
	if err != nil {
		return nil, err
	}

	offered := []string{}
	seen := map[string]bool{}
	for _, name := range names {
		mimeType, known := sdpNameToMimeType[name]
		if !known || seen[mimeType] {
			continue
		}
		seen[mimeType] = true
		offered = append(offered, mimeType)
	}

	return offered, nil
}

// OfferedAudioCodecs returns the rtpmap encoding names of the audio section,
// e.g. ["OPUS", "PCMU"]. Names are returned rather than mime types because the
// point is to report what a sender wanted when none of it can be forwarded.
func OfferedAudioCodecs(offer string) ([]string, error) {
	return encodingNames(offer, "audio")
}

// OffersOpus reports whether Opus is among the offered audio encodings. Opus is
// the only audio codec that can be forwarded, because the ghost audio track is
// always an Opus track.
func OffersOpus(audioEncodings []string) bool {
	for _, name := range audioEncodings {
		if name == "OPUS" {
			return true
		}
	}
	return false
}

// SelectVideoCodec picks the first configured codec the sender also offers. The
// server preference wins over the sender preference, which is what lets the
// meeting server get VP9 even when a sender would rather send H264.
func SelectVideoCodec(preference []VideoCodec, offered []string) (VideoCodec, bool) {
	for _, codec := range preference {
		for _, mimeType := range offered {
			if strings.EqualFold(codec.MimeType, mimeType) {
				return codec, true
			}
		}
	}
	return VideoCodec{}, false
}
