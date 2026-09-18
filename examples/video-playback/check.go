package main

import (
	"fmt"
	"math"
	"path/filepath"
	"sort"
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
	codecIDVP8:      "VP8",
	codecIDVP9:      "VP9",
	codecIDAV1:      "AV1",
	codecIDH264:     "H264",
	codecIDH265:     "H265",
	codecIDOpus:     "Opus",
	"A_AC3":         "AC-3",
	"A_EAC3":        "E-AC-3",
	"A_DTS":         "DTS",
	"A_MPEG/L3":     "MP3",
	"A_FLAC":        "FLAC",
	"A_PCM/INT/LIT": "PCM",
	codecIDVorbis:   "Vorbis",
	codecIDAAC:      "AAC",
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
		fmt.Printf("\n%s\n\n", filepath.Base(path))
		fmt.Printf("  CANNOT BE STREAMED\n")
		fmt.Printf("  %v\n\n", err)
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

	// Video. A codec the media server accepts but whose configuration could
	// not be read is a different problem from one it cannot take at all.
	_, knownCodec := supportedVideoCodecs[r.videoID]
	switch {
	case r.plan != nil:
		fmt.Printf("  Video       %s, supported\n", displayName(r.videoID))
	case knownCodec:
		fmt.Printf("  Video       %s, supported, but this file could not be read\n",
			displayName(r.videoID))
	default:
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
	var notes []string

	if r.plan == nil {
		fmt.Printf("  CANNOT BE STREAMED\n")
		fmt.Printf("  %v\n\n", r.planErr)
		return 1
	}

	if !r.hasAudio && r.audioID != "" {
		notes = append(notes, r.audioNote)
	}
	if r.fps > maxServerFPS+0.5 {
		notes = append(notes, fmt.Sprintf(
			"%.0f fps is above the 25 fps the media server supports", r.fps))
	}
	if r.maxKeyframeGap > longKeyframeGap {
		detail := fmt.Sprintf("the stream goes up to %s without a keyframe",
			r.maxKeyframeGap.Round(time.Second))
		if r.keyframes == 1 {
			detail = fmt.Sprintf("there is only one keyframe, at the start, leaving %s with no "+
				"recovery point", r.maxKeyframeGap.Round(time.Second))
		}
		notes = append(notes, detail+", so a lost packet can freeze the picture that long")
	}
	if r.spiky() {
		notes = append(notes, fmt.Sprintf(
			"one second peaks at %s, well above the rest of the file", mbit(r.peakBitrate)))
	}
	if r.maxFramePackets > burstyFrameManyPackets {
		notes = append(notes, fmt.Sprintf(
			"the largest frame needs %d RTP packets, which is a lot to send at once",
			r.maxFramePackets))
	}

	rate := r.sustained()
	exit := 0

	switch {
	case rate > bitrateMarginal:
		fmt.Printf("  TOO HEAVY TO STREAM\n")
		fmt.Printf("  A sustained %s is more than a meeting participant can send.\n", mbit(rate))
		fmt.Printf("  Expect little or no picture.\n")
		exit = 1
	case rate > bitrateComfortable:
		fmt.Printf("  SHOULD STREAM, WITH SOME RISK\n")
		fmt.Printf("  A sustained %s is on the high side. It will usually play, but a\n", mbit(rate))
		fmt.Printf("  lost packet can freeze the picture until the next keyframe.\n")
	default:
		fmt.Printf("  READY TO STREAM\n")
		fmt.Printf("  A sustained %s is comfortably within what a meeting participant\n  can send.\n", mbit(rate))
	}

	if len(notes) > 0 {
		fmt.Println()
		for _, n := range notes {
			fmt.Printf("  - %s\n", n)
		}
	}
	fmt.Println()
	return exit
}
