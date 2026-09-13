package main

import (
	"fmt"
	"math"
	"path/filepath"
	"time"

	"github.com/ebml-go/webm"
)

// Rough guidance for what one WebRTC participant can reasonably push into a
// meeting. These are not hard limits enforced anywhere, they are the
// thresholds used to phrase the verdict.
const (
	bitrateComfortable = 2_500_000
	bitrateMarginal    = 6_000_000

	// Above this many RTP packets, a single frame is a burst worth warning
	// about even when the average bitrate looks acceptable.
	burstyFrameManyPackets = 60

	// A lost packet freezes the picture until the next keyframe, so a long gap
	// between keyframes means a long freeze.
	longKeyframeGap = 5 * time.Second

	// Payload budget per RTP packet, mirroring rtpOutboundMTU minus the fixed
	// 12 byte RTP header.
	rtpPayloadBudget = 1200 - 12
)

// codecDisplayNames maps container CodecIDs to names people recognise.
var codecDisplayNames = map[string]string{
	codecIDVP8:    "VP8",
	codecIDVP9:    "VP9",
	codecIDAV1:    "AV1",
	codecIDH264:   "H264",
	codecIDH265:   "H265",
	codecIDOpus:   "Opus",
	codecIDVorbis: "Vorbis",
	codecIDAAC:    "AAC",
}

func displayName(codecID string) string {
	if name, ok := codecDisplayNames[codecID]; ok {
		return name
	}
	if codecID == "" {
		return "none"
	}
	return codecID
}

type checkReport struct {
	path      string
	docType   string
	duration  time.Duration
	width     uint
	height    uint
	videoID   string
	audioID   string
	hasAudio  bool
	audioNote string

	// nil when the video codec cannot be streamed at all
	plan    *streamPlan
	planErr error

	frames           int
	videoBytes       int64
	audioBytes       int64
	maxFrameBytes    int
	maxFramePackets  int
	packetsEstimated bool
	keyframes        int
	maxKeyframeGap   time.Duration
	peakBitrate      float64
	avgBitrate       float64
	fps              float64
}

// checkFile inspects a file and reports whether it can be streamed, without
// contacting the API or creating a meeting. Returns a process exit code.
func checkFile(path string) int {
	report, err := inspect(path)
	if err != nil {
		fmt.Printf("%s\n\n", filepath.Base(path))
		fmt.Printf("  Cannot read this file: %v\n\n", err)
		fmt.Printf("  Only WebM and Matroska containers are supported. Remux first:\n")
		fmt.Printf("    ffmpeg -i %s -c:v copy -c:a libopus out.mkv\n\n", filepath.Base(path))
		return 1
	}
	return report.print()
}

func inspect(path string) (*checkReport, error) {
	ctx, reader, cleanup, err := openWebM(path)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	videoTrack := ctx.FindFirstVideoTrack()
	if videoTrack == nil {
		return nil, fmt.Errorf("file contains no video track")
	}
	audioTrack := ctx.FindFirstAudioTrack()

	report := &checkReport{
		path:     path,
		docType:  ctx.DocType,
		duration: ctx.Segment.SegmentInformation.GetDuration(),
		width:    videoTrack.Video.PixelWidth,
		height:   videoTrack.Video.PixelHeight,
		videoID:  videoTrack.CodecID,
	}
	if audioTrack != nil {
		report.audioID = audioTrack.CodecID
	}

	// probe() is the same decision the player makes at startup, so reuse it
	// rather than duplicating the support rules. Its error is the reason.
	report.plan, report.planErr = probe(path, false)
	if report.plan != nil {
		report.hasAudio = report.plan.hasAudio
		report.audioNote = report.plan.audioSkipReason
	}

	var (
		lastTC        time.Duration
		lastKeyframe  time.Duration
		seenKeyframe  bool
		bytesPerSec   = map[int64]int64{}
		audioTrackNum uint
	)
	if audioTrack != nil {
		audioTrackNum = audioTrack.TrackNumber
	}

	for packet := range reader.Chan {
		if len(packet.Data) == 0 {
			break
		}
		if packet.Timecode == webm.BadTC {
			continue
		}
		second := int64(packet.Timecode / time.Second)

		switch packet.TrackNumber {
		case videoTrack.TrackNumber:
			report.frames++
			report.videoBytes += int64(len(packet.Data))
			bytesPerSec[second] += int64(len(packet.Data))
			if packet.Timecode > lastTC {
				lastTC = packet.Timecode
			}
			if packet.Keyframe {
				report.keyframes++
				if seenKeyframe {
					if gap := packet.Timecode - lastKeyframe; gap > report.maxKeyframeGap {
						report.maxKeyframeGap = gap
					}
				}
				lastKeyframe = packet.Timecode
				seenKeyframe = true
			}
			report.countPackets(packet)

		case audioTrackNum:
			if audioTrack == nil {
				continue
			}
			report.audioBytes += int64(len(packet.Data))
			bytesPerSec[second] += int64(len(packet.Data))
		}
	}

	// The stretch after the final keyframe counts too: a file with a single
	// keyframe at the start has no recovery point at all, which would
	// otherwise be reported as a gap of zero.
	if seenKeyframe {
		if gap := lastTC - lastKeyframe; gap > report.maxKeyframeGap {
			report.maxKeyframeGap = gap
		}
	}

	// The header duration is only second-accurate, so prefer what we measured.
	if lastTC > report.duration {
		report.duration = lastTC
	}
	if report.duration > 0 {
		seconds := report.duration.Seconds()
		report.avgBitrate = float64(report.videoBytes+report.audioBytes) * 8 / seconds
		report.fps = float64(report.frames) / seconds
	}
	for _, b := range bytesPerSec {
		if rate := float64(b) * 8; rate > report.peakBitrate {
			report.peakBitrate = rate
		}
	}
	return report, nil
}

// countPackets tracks the largest frame and how many RTP packets it becomes.
// When the codec is supported the real payloader is used, so the number
// matches what the player will actually put on the wire.
func (r *checkReport) countPackets(packet webm.Packet) {
	if len(packet.Data) <= r.maxFrameBytes {
		return
	}
	r.maxFrameBytes = len(packet.Data)

	if r.plan == nil || r.plan.videoPayloader == nil {
		r.maxFramePackets = int(math.Ceil(float64(len(packet.Data)) / rtpPayloadBudget))
		r.packetsEstimated = true
		return
	}
	data := packet.Data
	if r.plan.videoRewrite != nil {
		data = r.plan.videoRewrite(data, packet.Keyframe)
	}
	r.maxFramePackets = len(r.plan.videoPayloader.Payload(rtpPayloadBudget, data))
}

func mbit(bitsPerSecond float64) string {
	return fmt.Sprintf("%.1f Mbit/s", bitsPerSecond/1e6)
}

func (r *checkReport) print() int {
	fmt.Printf("\n%s\n\n", filepath.Base(r.path))

	container := r.docType
	if container == "" {
		container = "matroska"
	}
	fmt.Printf("  Container   %s, %s\n", container, r.duration.Round(time.Second))
	fmt.Printf("  Resolution  %dx%d", r.width, r.height)
	if r.fps > 0 {
		fmt.Printf(" @ %.1f fps", r.fps)
	}
	fmt.Println()

	// Video
	if r.plan != nil {
		fmt.Printf("  Video       %s, supported\n", displayName(r.videoID))
	} else {
		fmt.Printf("  Video       %s, NOT supported\n", displayName(r.videoID))
	}

	// Audio
	switch {
	case r.audioID == "":
		fmt.Printf("  Audio       none\n")
	case r.hasAudio:
		fmt.Printf("  Audio       %s, supported\n", displayName(r.audioID))
	default:
		fmt.Printf("  Audio       %s, NOT supported, will stream without audio\n",
			displayName(r.audioID))
	}

	// Bitrate
	fmt.Printf("  Bitrate     %s average, %s peak second\n",
		mbit(r.avgBitrate), mbit(r.peakBitrate))
	if r.maxFramePackets > 0 {
		qualifier := ""
		if r.packetsEstimated {
			qualifier = " estimated"
		}
		fmt.Printf("  Worst frame %d KB, %d RTP packets%s\n",
			r.maxFrameBytes/1024, r.maxFramePackets, qualifier)
	}
	if r.keyframes == 0 {
		fmt.Printf("  Keyframes   none found\n")
	} else {
		fmt.Printf("  Keyframes   %d, longest stretch without one %s\n",
			r.keyframes, r.maxKeyframeGap.Round(100*time.Millisecond))
	}

	fmt.Println()
	return r.verdict()
}

func (r *checkReport) verdict() int {
	var warnings []string

	if r.plan == nil {
		fmt.Printf("  WILL NOT STREAM\n")
		fmt.Printf("  %v\n\n", r.planErr)
		fmt.Printf("  Re-encode the video:\n")
		fmt.Printf("    %s\n\n", suggestedCommand(r, true))
		return 1
	}

	if !r.hasAudio && r.audioID != "" {
		warnings = append(warnings, r.audioNote)
	}
	if r.maxKeyframeGap > longKeyframeGap {
		detail := fmt.Sprintf("the stream goes up to %s without a keyframe",
			r.maxKeyframeGap.Round(time.Second))
		if r.keyframes == 1 {
			detail = fmt.Sprintf("there is only one keyframe, at the start, leaving %s with no "+
				"recovery point", r.maxKeyframeGap.Round(time.Second))
		}
		warnings = append(warnings,
			detail+", so a lost packet can freeze the picture that long")
	}
	if r.maxFramePackets > burstyFrameManyPackets {
		warnings = append(warnings, fmt.Sprintf(
			"the largest frame needs %d RTP packets, which is a heavy burst",
			r.maxFramePackets))
	}

	rate := math.Max(r.peakBitrate, r.avgBitrate)
	exit := 0

	switch {
	case rate > bitrateMarginal:
		fmt.Printf("  NOT RECOMMENDED\n")
		fmt.Printf("  At %s this is far above what a meeting participant can send.\n", mbit(rate))
		fmt.Printf("  Expect little or no picture. Re-encode before streaming.\n")
		exit = 1
	case rate > bitrateComfortable:
		fmt.Printf("  SHOULD WORK, WITH RISK\n")
		fmt.Printf("  At %s this is on the high side. It will usually play, but a\n", mbit(rate))
		fmt.Printf("  lost packet can freeze the picture until the next keyframe.\n")
	default:
		fmt.Printf("  LOOKS GOOD\n")
		fmt.Printf("  %s is comfortably within what a meeting participant can send.\n", mbit(rate))
	}

	if len(warnings) > 0 {
		fmt.Println()
		for _, w := range warnings {
			fmt.Printf("  - %s\n", w)
		}
	}

	if exit != 0 || len(warnings) > 0 {
		fmt.Printf("\n  Suggested:\n    %s\n", suggestedCommand(r, false))
	}
	fmt.Println()
	return exit
}

// suggestedCommand builds an ffmpeg line that fixes whatever is wrong. The
// video is only re-encoded when it has to be, since copying is far cheaper.
func suggestedCommand(r *checkReport, mustTranscodeVideo bool) string {
	in := filepath.Base(r.path)

	videoArgs := "-c:v copy"
	if mustTranscodeVideo {
		videoArgs = "-c:v libvpx-vp9 -b:v 2M -maxrate 2.5M -g 60"
	} else if math.Max(r.peakBitrate, r.avgBitrate) > bitrateComfortable {
		videoArgs = "-c:v libvpx-vp9 -b:v 2M -maxrate 2.5M -g 60"
	} else if r.maxKeyframeGap > longKeyframeGap {
		// Bitrate is fine, only the keyframe spacing needs fixing, and that
		// still requires a re-encode.
		videoArgs = "-c:v libvpx-vp9 -b:v 2M -g 60"
	}

	audioArgs := "-an"
	if r.audioID != "" {
		if r.hasAudio {
			audioArgs = "-c:a copy"
		} else {
			audioArgs = "-c:a libopus"
		}
	}

	out := "out.webm"
	if ext := filepath.Ext(in); ext == ".mkv" {
		out = "out.mkv"
	}
	return fmt.Sprintf("ffmpeg -i %s %s %s %s", in, videoArgs, audioArgs, out)
}
