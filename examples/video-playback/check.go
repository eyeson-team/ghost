package main

import (
	"fmt"
	"path/filepath"
	"time"
)

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
	path     string
	docType  string
	duration time.Duration
	width    uint
	height   uint
	fps      float64
	videoID  string
	audioID  string
	hasAudio bool

	plan    *streamPlan
	planErr error
}

// checkFile returns the exit code: 1 if the file is not playable.
func checkFile(path string) int {
	report, err := inspect(path)
	if err != nil {
		fmt.Printf("\n%s\n\n", filepath.Base(path))
		fmt.Printf("  NOT PLAYABLE\n")
		fmt.Printf("  %v\n\n", err)
		return 1
	}
	return report.print()
}

func inspect(path string) (*checkReport, error) {
	ctx, _, cleanup, err := openWebM(path)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	videoTrack := ctx.FindFirstVideoTrack()
	if videoTrack == nil {
		return nil, fmt.Errorf("file contains no video track")
	}

	report := &checkReport{
		path:     path,
		docType:  ctx.DocType,
		duration: ctx.Segment.SegmentInformation.GetDuration(),
		width:    videoTrack.Video.PixelWidth,
		height:   videoTrack.Video.PixelHeight,
		videoID:  videoTrack.CodecID,
	}
	// Optional in the header.
	if d := videoTrack.GetDefaultDuration(); d > 0 {
		report.fps = float64(time.Second) / float64(d)
	}
	if audioTrack := ctx.FindFirstAudioTrack(); audioTrack != nil {
		report.audioID = audioTrack.CodecID
	}

	report.plan, report.planErr = probe(path, false)
	if report.plan != nil {
		report.hasAudio = report.plan.hasAudio
	}
	return report, nil
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
		fmt.Printf(" @ %.2f fps", r.fps)
	}
	fmt.Println()

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

	switch {
	case r.audioID == "":
		fmt.Printf("  Audio       none\n")
	case r.hasAudio:
		fmt.Printf("  Audio       %s, supported\n", displayName(r.audioID))
	default:
		fmt.Printf("  Audio       %s, NOT supported, will play without audio\n",
			displayName(r.audioID))
	}

	fmt.Println()
	if r.plan == nil {
		fmt.Printf("  NOT PLAYABLE\n")
		fmt.Printf("  %v\n\n", r.planErr)
		return 1
	}
	fmt.Printf("  PLAYABLE\n\n")
	return 0
}