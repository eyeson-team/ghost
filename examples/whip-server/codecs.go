package main

import (
	"fmt"
	"sort"
	"strings"

	ghost "github.com/eyeson-team/ghost/v2"
	"github.com/pion/sdp/v3"
	"github.com/pion/webrtc/v3"
)

type VideoCodec struct {
	Name        string
	MimeType    string
	GhostOption ghost.ClientOption

	// Sent in the answer to pin the sender to one format.
	FmtpLine string

	Accepts func(parameters map[string]string) bool
}

// VP9 first, the eyeson media server prefers it.
const DefaultVideoCodecs = "vp9,av1,vp8,h264,h265"

// The only VP9 profile the eyeson media server decodes.
const vp9Profile0 = "profile-id=0"

var knownVideoCodecs = []VideoCodec{
	{Name: "vp9", MimeType: webrtc.MimeTypeVP9, GhostOption: ghost.WithForceVP9Codec(),
		FmtpLine: vp9Profile0, Accepts: isVP9Profile0},
	{Name: "av1", MimeType: webrtc.MimeTypeAV1, GhostOption: ghost.WithForceAV1Codec()},
	{Name: "vp8", MimeType: webrtc.MimeTypeVP8, GhostOption: ghost.WithForceVP8Codec()},
	{Name: "h264", MimeType: webrtc.MimeTypeH264, GhostOption: ghost.WithForceH264Codec()},
	{Name: "h265", MimeType: webrtc.MimeTypeH265, GhostOption: ghost.WithForceH265Codec()},
}

// A missing profile-id means profile 0.
func isVP9Profile0(parameters map[string]string) bool {
	profile, given := parameters["profile-id"]
	return !given || strings.TrimSpace(profile) == "0"
}

// AV1X: older Chrome. HEVC: some senders' name for H265.
var sdpNameToMimeType = map[string]string{
	"H264": webrtc.MimeTypeH264,
	"VP8":  webrtc.MimeTypeVP8,
	"VP9":  webrtc.MimeTypeVP9,
	"AV1":  webrtc.MimeTypeAV1,
	"AV1X": webrtc.MimeTypeAV1,
	"H265": webrtc.MimeTypeH265,
	"HEVC": webrtc.MimeTypeH265,
}

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

type VideoFormat struct {
	MimeType   string
	Parameters map[string]string
}

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

type offeredFormat struct {
	Name       string
	Parameters map[string]string
}

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

// Skips rtx, red, fec and unknown encodings.
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

func OfferedVideoCodecs(offer string) ([]string, error) {
	formats, err := OfferedVideoFormats(offer)
	if err != nil {
		return nil, err
	}
	return VideoMimeTypes(formats), nil
}

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

func OffersOpus(audioEncodings []string) bool {
	for _, name := range audioEncodings {
		if name == "OPUS" {
			return true
		}
	}
	return false
}

func (c VideoCodec) accepts(format VideoFormat) bool {
	if !strings.EqualFold(c.MimeType, format.MimeType) {
		return false
	}
	if c.Accepts == nil {
		return true
	}
	return c.Accepts(format.Parameters)
}

// Server preference wins over sender preference.
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

// Codecs that were offered, but only in a format that cannot be forwarded.
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
