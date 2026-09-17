package main

import (
	"fmt"
	"math"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/ebml-go/webm"
)

// Rough guidance for what one WebRTC participant can reasonably push into a
// meeting. These are not hard limits enforced anywhere, they are the
// thresholds used to phrase the verdict.
// Thresholds calibrated against files actually streamed into a meeting:
//
//	1.0 Mbit/s sustained, 1280x720 VP9        clean
//	1.5 Mbit/s sustained, 1280x720 VP9        clean
//	3.1 Mbit/s sustained, 1280x534 H264       clean, 140 packet frames
//	3.3 Mbit/s sustained, 1920x800 VP9        clean video, 614 packet frames
//	11.2 Mbit/s sustained, 1920x800 VP8       no usable picture
//	46.1 Mbit/s sustained, 1280x534 VP9       no picture
//
// So anything up to a bit over 3 Mbit/s is known good and the failures start
// an order of magnitude higher. The comfortable ceiling sits above the
// measured good cases with room to spare, and the marginal band covers the
// untested gap rather than pretending to know where exactly it breaks.
const (
	bitrateComfortable = 4_000_000
	bitrateMarginal    = 8_000_000

	// Frames of this many RTP packets are worth mentioning, but note that the
	// player paces a frame's packets across its interval, and files with 614
	// packet frames have streamed cleanly. This is a heads-up, not a fault:
	// no failure has yet been traced to burst size alone since pacing landed.
	burstyFrameManyPackets = 400

	// A lost packet freezes the picture until the next keyframe, so a long gap
	// between keyframes means a long freeze.
	longKeyframeGap = 5 * time.Second

	// The eyeson media server tops out at 25 fps, and anything above it is
	// bitrate spent on frames that will not be shown.
	maxServerFPS = 25.0

	// A meeting tile is nowhere near 1080p wide. Downscaling is the most
	// effective single lever on both bitrate and keyframe size.
	preferredWidth = 1280

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
	p95Bitrate       float64
	avgBitrate       float64
	fps              float64
	fixedTimecodes   int
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
		fixer         = &timecodeFixer{}
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
		tc := fixer.fix(packet.Timecode)
		second := int64(tc / time.Second)

		switch packet.TrackNumber {
		case videoTrack.TrackNumber:
			report.frames++
			report.videoBytes += int64(len(packet.Data))
			bytesPerSec[second] += int64(len(packet.Data))
			if tc > lastTC {
				lastTC = tc
			}
			if packet.Keyframe {
				report.keyframes++
				if seenKeyframe {
					if gap := tc - lastKeyframe; gap > report.maxKeyframeGap {
						report.maxKeyframeGap = gap
					}
				}
				lastKeyframe = tc
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

	report.fixedTimecodes = fixer.fixed

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
	report.peakBitrate, report.p95Bitrate = bitrateStats(bytesPerSec)
	return report, nil
}

// bitrateStats returns the busiest second and the 95th percentile second.
//
// The percentile matters more than the peak for deciding whether a file is
// streamable. A feature film contains scene cuts, and the keyframe at a cut
// can be many times the size of an ordinary frame, so one second in a
// perfectly reasonable 2 Mbit/s file can measure ten times that. Judging the
// whole file by that second condemns files that play fine apart from a brief
// glitch at the cut.
func bitrateStats(bytesPerSec map[int64]int64) (peak, p95 float64) {
	if len(bytesPerSec) == 0 {
		return 0, 0
	}
	rates := make([]float64, 0, len(bytesPerSec))
	for _, b := range bytesPerSec {
		rates = append(rates, float64(b)*8)
	}
	sort.Float64s(rates)

	peak = rates[len(rates)-1]
	idx := int(math.Ceil(0.95*float64(len(rates)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(rates) {
		idx = len(rates) - 1
	}
	return peak, rates[idx]
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

// sustained is the rate the verdict is based on: what the file asks for
// continuously, rather than its single worst moment.
func (r *checkReport) sustained() float64 {
	return math.Max(r.avgBitrate, r.p95Bitrate)
}

// needsAdvice reports whether the file has anything worth acting on. A file
// that streams cleanly should be left alone rather than handed an ffmpeg line.
func (r *checkReport) needsAdvice() bool {
	return r.plan == nil ||
		r.sustained() > bitrateComfortable ||
		r.spiky() ||
		r.maxKeyframeGap > longKeyframeGap ||
		r.fps > maxServerFPS+0.5 ||
		(!r.hasAudio && r.audioID != "") ||
		r.maxFramePackets > burstyFrameManyPackets
}

// spiky reports whether one second stands far above the rest of the file.
func (r *checkReport) spiky() bool {
	return r.peakBitrate > 2.5*r.sustained() && r.peakBitrate > bitrateMarginal
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
	fmt.Printf("  Bitrate     %s average, %s sustained (95th percentile second)\n",
		mbit(r.avgBitrate), mbit(r.p95Bitrate))
	qualifier := ""
	if r.packetsEstimated {
		qualifier = " estimated"
	}
	fmt.Printf("  Worst case  %s in one second, largest frame %d KB (%d RTP packets%s)\n",
		mbit(r.peakBitrate), r.maxFrameBytes/1024, r.maxFramePackets, qualifier)
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
	if r.fps > maxServerFPS+0.5 {
		warnings = append(warnings, fmt.Sprintf(
			"%.0f fps is above the 25 fps the media server supports, so some of the bitrate "+
				"is spent on frames that will not be shown", r.fps))
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
			"the largest frame needs %d RTP packets; the player paces these out, but it is "+
				"a lot to push at once", r.maxFramePackets))
	}

	rate := r.sustained()
	exit := 0

	// A short spike is a glitch, not a reason to reject the file.
	if r.spiky() {
		warnings = append(warnings, fmt.Sprintf(
			"one second peaks at %s, far above the rest of the file, so expect a brief "+
				"stutter there rather than a problem throughout", mbit(r.peakBitrate)))
	}

	switch {
	case rate > bitrateMarginal:
		fmt.Printf("  NOT RECOMMENDED\n")
		fmt.Printf("  At a sustained %s this is far above what a meeting participant\n", mbit(rate))
		fmt.Printf("  can send. Expect little or no picture. Re-encode before streaming.\n")
		exit = 1
	case rate > bitrateComfortable:
		fmt.Printf("  SHOULD WORK, WITH RISK\n")
		fmt.Printf("  At a sustained %s this is on the high side. It will usually play,\n", mbit(rate))
		fmt.Printf("  but a lost packet can freeze the picture until the next keyframe.\n")
	default:
		fmt.Printf("  LOOKS GOOD\n")
		fmt.Printf("  A sustained %s is comfortably within what a meeting participant\n  can send.\n", mbit(rate))
	}

	if len(warnings) > 0 {
		fmt.Println()
		for _, w := range warnings {
			fmt.Printf("  - %s\n", w)
		}
	}

	if r.needsAdvice() {
		fmt.Printf("\n  Suggested:\n    %s\n", suggestedCommand(r, false))
	}
	fmt.Println()
	return exit
}

// suggestedCommand builds an ffmpeg line that fixes whatever is actually
// wrong, and only re-encodes what has to be re-encoded.
//
// H264 via x264 is the default when the video does need re-encoding. libvpx-vp9
// in single pass VBR is both very slow and unreliable about hitting a bitrate
// target, to the point of overshooting it by an order of magnitude, whereas
// x264 holds the target and encodes many times faster. Baseline profile keeps
// B-frames out of the stream, which keeps the timestamps monotonic.
//
// Resolution and frame rate come before any bitrate flag: they cut both the
// sustained rate and the size of individual keyframes.
func suggestedCommand(r *checkReport, mustTranscodeVideo bool) string {
	in := filepath.Base(r.path)

	reencode := mustTranscodeVideo ||
		r.sustained() > bitrateComfortable ||
		r.spiky() ||
		r.maxKeyframeGap > longKeyframeGap ||
		r.fps > maxServerFPS+0.5

	parts := []string{"ffmpeg", "-i", in}
	out := "out" + filepath.Ext(in)
	if out == "out" {
		out = "out.webm"
	}

	if !reencode {
		parts = append(parts, "-c:v", "copy")
	} else {
		if r.width > preferredWidth {
			parts = append(parts, "-vf", fmt.Sprintf("scale=%d:-2", preferredWidth))
		}
		if r.fps > maxServerFPS+0.5 {
			parts = append(parts, "-r", "25")
		}
		parts = append(parts,
			"-c:v", "libx264", "-preset", "veryfast", "-profile:v", "baseline",
			"-b:v", "2M", "-maxrate", "2.5M", "-bufsize", "4M",
			"-g", "50", "-pix_fmt", "yuv420p")
		// H264 cannot go in a .webm container.
		out = "out.mkv"
	}

	switch {
	case r.audioID == "":
		parts = append(parts, "-an")
	case r.hasAudio && out == "out.mkv" && filepath.Ext(in) == ".webm":
		// Opus copies cleanly into Matroska.
		parts = append(parts, "-c:a", "copy")
	case r.hasAudio:
		parts = append(parts, "-c:a", "copy")
	default:
		parts = append(parts, "-c:a", "libopus", "-b:a", "96k", "-ac", "1")
	}

	return strings.Join(append(parts, out), " ")
}
