package main

import (
	"fmt"
	"sort"
	"strings"

	ghost "github.com/eyeson-team/ghost/v2"
	"github.com/pion/sdp/v3"
	"github.com/pion/webrtc/v3"
)

// VideoCodec ties together the three names one codec has in this example: the
// name used on the command line, the mime type used by pion and in SDP, and the
// ghost option that makes the eyeson side negotiate it. Codecs the meeting
// server only decodes in one flavour carry that restriction as well.
type VideoCodec struct {
	Name        string
	MimeType    string
	GhostOption ghost.ClientOption

	// FmtpLine is registered with pion and ends up in the answer, which is how
	// a sender that offers several formats of this codec is told which one to
	// send. Empty means the sender's format is answered as offered.
	FmtpLine string

	// Accepts reports whether one offered format of this codec can be
	// forwarded. A nil Accepts takes every format.
	Accepts func(parameters map[string]string) bool
}

// DefaultVideoCodecs is the preference order used when nothing else is given.
// The eyeson media server prefers VP9, so that is tried first, and the two
// codecs every WHIP sender can fall back on come last: H264, then H265.
const DefaultVideoCodecs = "vp9,av1,vp8,h264,h265"

// vp9Profile0 is the only VP9 profile the eyeson media server decodes: 8 bit,
// 4:2:0. Senders that can do more than one (Chrome offers profile 0 and profile
// 2) are pinned to it by the fmtp line in the answer; senders that can only do
// another one are turned down before a session is set up, because nothing in
// the forwarding path would notice the mismatch afterwards.
const vp9Profile0 = "profile-id=0"

var knownVideoCodecs = []VideoCodec{
	{Name: "vp9", MimeType: webrtc.MimeTypeVP9, GhostOption: ghost.WithForceVP9Codec(),
		FmtpLine: vp9Profile0, Accepts: isVP9Profile0},
	{Name: "av1", MimeType: webrtc.MimeTypeAV1, GhostOption: ghost.WithForceAV1Codec()},
	{Name: "vp8", MimeType: webrtc.MimeTypeVP8, GhostOption: ghost.WithForceVP8Codec()},
	{Name: "h264", MimeType: webrtc.MimeTypeH264, GhostOption: ghost.WithForceH264Codec()},
	{Name: "h265", MimeType: webrtc.MimeTypeH265, GhostOption: ghost.WithForceH265Codec()},
}

// isVP9Profile0 reports whether an offered VP9 format is profile 0. profile-id
// may be left out of an offer, in which case profile 0 is to be inferred - the
// VP9 RTP payload format says so, and pion's own fmtp matching does the same.
func isVP9Profile0(parameters map[string]string) bool {
	profile, given := parameters["profile-id"]
	return !given || strings.TrimSpace(profile) == "0"
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

// VideoFormat is one video payload type of an SDP offer: which codec it is and
// how it is parameterised. The parameters are kept because a sender may offer
// the same codec more than once - Chrome offers VP9 as profile 0 and as profile
// 2 - and only some of those can be forwarded.
type VideoFormat struct {
	MimeType   string
	Parameters map[string]string
}

// Describe renders the fmtp parameters in a stable order, for log lines.
func (f VideoFormat) Describe() string {
	if len(f.Parameters) == 0 {
		return "no fmtp line"
	}
	parts := make([]string, 0, len(f.Parameters))
	for key, value := range f.Parameters {
		if value == "" {
			parts = append(parts, key)
			continue
		}
		parts = append(parts, key+"="+value)
	}
	sort.Strings(parts)
	return strings.Join(parts, ";")
}

// offeredFormat is one payload type of an SDP offer, before it is mapped onto a
// pion mime type: the rtpmap encoding name plus the parameters of its fmtp line.
type offeredFormat struct {
	Name       string
	Parameters map[string]string
}

// offeredFormats returns the payload types of one media kind ("video" or
// "audio") in an SDP offer, in the order the sender listed them.
func offeredFormats(offer, kind string) ([]offeredFormat, error) {
	parsed := sdp.SessionDescription{}
	if err := parsed.Unmarshal([]byte(offer)); err != nil {
		return nil, err
	}

	formats := []offeredFormat{}

	for _, media := range parsed.MediaDescriptions {
		if media.MediaName.Media != kind {
			continue
		}

		order := []string{}
		names := map[string]string{}
		parameters := map[string]map[string]string{}

		for _, attribute := range media.Attributes {
			switch attribute.Key {
			case "rtpmap":
				// a=rtpmap:96 VP8/90000
				payloadType, encoding, found := strings.Cut(attribute.Value, " ")
				if !found {
					continue
				}
				if _, seen := names[payloadType]; seen {
					continue
				}
				names[payloadType] = strings.ToUpper(strings.Split(encoding, "/")[0])
				order = append(order, payloadType)
			case "fmtp":
				// a=fmtp:98 profile-id=2
				payloadType, line, found := strings.Cut(attribute.Value, " ")
				if !found {
					continue
				}
				parameters[payloadType] = parseFmtpParameters(line)
			}
		}

		for _, payloadType := range order {
			formats = append(formats, offeredFormat{
				Name:       names[payloadType],
				Parameters: parameters[payloadType],
			})
		}
	}

	return formats, nil
}

// parseFmtpParameters splits an fmtp line into its key/value pairs, the same way
// pion does it internally.
func parseFmtpParameters(line string) map[string]string {
	parameters := map[string]string{}
	for _, part := range strings.Split(line, ";") {
		key, value, _ := strings.Cut(strings.TrimSpace(part), "=")
		key = strings.ToLower(strings.TrimSpace(key))
		if key == "" {
			continue
		}
		parameters[key] = strings.TrimSpace(value)
	}
	return parameters
}

// OfferedVideoFormats returns every video format found in an SDP offer, in the
// order the sender listed them. Unknown encodings and the helper formats (rtx,
// red, ulpfec, flexfec) are left out.
func OfferedVideoFormats(offer string) ([]VideoFormat, error) {
	formats, err := offeredFormats(offer, "video")
	if err != nil {
		return nil, err
	}

	offered := []VideoFormat{}
	for _, format := range formats {
		mimeType, known := sdpNameToMimeType[format.Name]
		if !known {
			continue
		}
		offered = append(offered, VideoFormat{
			MimeType:   mimeType,
			Parameters: format.Parameters,
		})
	}

	return offered, nil
}

// VideoMimeTypes lists the distinct mime types of a set of formats, keeping the
// order they came in. That is what a log line wants - the profiles only get
// interesting when something is turned down because of them.
func VideoMimeTypes(formats []VideoFormat) []string {
	mimeTypes := []string{}
	seen := map[string]bool{}
	for _, format := range formats {
		if seen[format.MimeType] {
			continue
		}
		seen[format.MimeType] = true
		mimeTypes = append(mimeTypes, format.MimeType)
	}
	return mimeTypes
}

// OfferedVideoCodecs returns the mime types of all video codecs found in an SDP
// offer, in the order the sender listed them. Profiles are not considered here,
// OfferedVideoFormats keeps them.
func OfferedVideoCodecs(offer string) ([]string, error) {
	formats, err := OfferedVideoFormats(offer)
	if err != nil {
		return nil, err
	}
	return VideoMimeTypes(formats), nil
}

// OfferedAudioCodecs returns the rtpmap encoding names of the audio section,
// e.g. ["OPUS", "PCMU"]. Names are returned rather than mime types because the
// point is to report what a sender wanted when none of it can be forwarded.
func OfferedAudioCodecs(offer string) ([]string, error) {
	formats, err := offeredFormats(offer, "audio")
	if err != nil {
		return nil, err
	}

	names := []string{}
	seen := map[string]bool{}
	for _, format := range formats {
		if seen[format.Name] {
			continue
		}
		seen[format.Name] = true
		names = append(names, format.Name)
	}

	return names, nil
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

// accepts reports whether one offered format can be forwarded as this codec.
func (c VideoCodec) accepts(format VideoFormat) bool {
	if !strings.EqualFold(c.MimeType, format.MimeType) {
		return false
	}
	if c.Accepts == nil {
		return true
	}
	return c.Accepts(format.Parameters)
}

// SelectVideoCodec picks the first configured codec the sender also offers in a
// usable format. The server preference wins over the sender preference, which is
// what lets the meeting server get VP9 even when a sender would rather send
// H264.
func SelectVideoCodec(preference []VideoCodec, offered []VideoFormat) (VideoCodec, bool) {
	for _, codec := range preference {
		for _, format := range offered {
			if codec.accepts(format) {
				return codec, true
			}
		}
	}
	return VideoCodec{}, false
}

// UnusableVideoFormats describes the configured codecs a sender did offer, but
// only in a format that cannot be forwarded - VP9 in a profile other than 0,
// today. Without it such an offer looks exactly like a sender that never
// mentioned the codec at all.
func UnusableVideoFormats(preference []VideoCodec, offered []VideoFormat) []string {
	unusable := []string{}

	for _, codec := range preference {
		usable := false
		rejected := []string{}

		for _, format := range offered {
			if !strings.EqualFold(codec.MimeType, format.MimeType) {
				continue
			}
			if codec.accepts(format) {
				usable = true
				break
			}
			rejected = append(rejected, format.Describe())
		}

		if usable || len(rejected) == 0 {
			continue
		}
		unusable = append(unusable, fmt.Sprintf("%s (%s)", codec.Name,
			strings.Join(rejected, ", ")))
	}

	return unusable
}