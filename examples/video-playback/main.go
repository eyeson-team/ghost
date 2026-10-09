package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	stdlog "log"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ebml-go/webm"
	"github.com/eyeson-team/eyeson-go"
	"github.com/eyeson-team/ghost/v2"
	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/pion/rtp/codecs/av1/obu"
	"github.com/rs/zerolog"
	log "github.com/rs/zerolog/log"
	"github.com/spf13/cobra"
)

var (
	Version = "dev"
)

var (
	apiEndpointFlag        string
	userFlag               string
	userIDFlag             string
	roomIDFlag             string
	verboseFlag            bool
	traceFlag              bool
	customCAFileFlag       string
	widescreenFlag         bool
	quietFlag              bool
	loopFlag               bool
	insecureSkipVerifyFlag bool
	noAudioFlag            bool
	startDelayFlag         time.Duration

	rootCommand = &cobra.Command{
		Use:   "ghost-player [flags] $API_KEY|$GUEST_LINK VIDEO_FILE",
		Short: "ghost-player",
		Args:  cobra.ExactArgs(2),
		Run: func(cmd *cobra.Command, args []string) {
			videoPlayerExample(args[0], args[1], apiEndpointFlag,
				userFlag, roomIDFlag, userIDFlag)
		},
	}
)

// https://www.matroska.org/technical/codec_specs.html
const (
	codecIDVP8    = "V_VP8"
	codecIDVP9    = "V_VP9"
	codecIDAV1    = "V_AV1"
	codecIDH264   = "V_MPEG4/ISO/AVC"
	codecIDH265   = "V_MPEGH/ISO/HEVC"
	codecIDOpus   = "A_OPUS"
)

const (
	videoClockRate = 90000
	opusClockRate  = 48000

	pacingThreshold     = 8
	pacingSlice         = time.Millisecond
	defaultPacingBudget = 20 * time.Millisecond
	maxPacingBudget     = 100 * time.Millisecond

	terminateTimeout = 5 * time.Second

	videoQueueDepth = 8
	audioQueueDepth = 50
)

type frameRewriter func(frame []byte, keyframe bool) []byte

type videoSupport struct {
	ghostOption ghost.ClientOption
	build       func(codecPrivate []byte) (rtp.Payloader, frameRewriter, error)
}

var supportedVideoCodecs = map[string]videoSupport{
	codecIDVP8: {
		ghostOption: ghost.WithForceVP8Codec(),
		build: func([]byte) (rtp.Payloader, frameRewriter, error) {
			return &codecs.VP8Payloader{}, nil, nil
		},
	},
	codecIDVP9: {
		ghostOption: ghost.WithForceVP9Codec(),
		build: func([]byte) (rtp.Payloader, frameRewriter, error) {
			return &codecs.VP9Payloader{}, nil, nil
		},
	},
	codecIDAV1: {
		ghostOption: ghost.WithForceAV1Codec(),
		build: func([]byte) (rtp.Payloader, frameRewriter, error) {
			return &av1TemporalUnitPayloader{}, nil, nil
		},
	},
	codecIDH264: {
		ghostOption: ghost.WithForceH264Codec(),
		build: func(codecPrivate []byte) (rtp.Payloader, frameRewriter, error) {
			rewrite, err := newAVCCToAnnexB(codecPrivate)
			if err != nil {
				return nil, nil, err
			}
			return &codecs.H264Payloader{}, rewrite, nil
		},
	},
	codecIDH265: {
		ghostOption: ghost.WithForceH265Codec(),
		build: func(codecPrivate []byte) (rtp.Payloader, frameRewriter, error) {
			rewrite, err := newHVCCToAnnexB(codecPrivate)
			if err != nil {
				return nil, nil, err
			}
			return &codecs.H265Payloader{}, rewrite, nil
		},
	},
}

const (
	obuTypeTemporalDelimiter = 2
	obuTypePadding           = 15

	obuExtensionFlagMask = 0x04
	obuHasSizeFieldMask  = 0x02
	obuTypeMask          = 0x78
	obuTypeShift         = 3
)

// pion's AV1Payloader panics on more than one OBU.
type av1TemporalUnitPayloader struct {
	inner codecs.AV1Payloader
}

func (p *av1TemporalUnitPayloader) Payload(mtu uint16, temporalUnit []byte) [][]byte {
	var payloads [][]byte
	for _, unit := range splitOBUs(temporalUnit) {
		payloads = append(payloads, p.inner.Payload(mtu, unit)...)
	}
	return payloads
}

// The AV1 RTP spec forbids temporal delimiters and padding.
func splitOBUs(temporalUnit []byte) [][]byte {
	var obus [][]byte
	for i := 0; i < len(temporalUnit); {
		header := temporalUnit[i]
		headerSize := 1
		if header&obuExtensionFlagMask != 0 {
			headerSize++
		}
		if i+headerSize > len(temporalUnit) {
			break
		}

		if header&obuHasSizeFieldMask == 0 {
			obus = append(obus, temporalUnit[i:])
			break
		}

		size, lebLength, err := obu.ReadLeb128(temporalUnit[i+headerSize:])
		if err != nil {
			break
		}
		end := i + headerSize + int(lebLength) + int(size)
		if end > len(temporalUnit) || end <= i {
			break
		}

		switch (header & obuTypeMask) >> obuTypeShift {
		case obuTypeTemporalDelimiter, obuTypePadding:
		default:
			obus = append(obus, temporalUnit[i:end])
		}
		i = end
	}
	return obus
}

var annexBStartCode = []byte{0x00, 0x00, 0x00, 0x01}

func newAVCCToAnnexB(codecPrivate []byte) (frameRewriter, error) {
	if len(codecPrivate) < 7 || codecPrivate[0] != 1 {
		return nil, fmt.Errorf("h264: invalid or missing avcC CodecPrivate (%d bytes)", len(codecPrivate))
	}
	nalLengthSize := int(codecPrivate[4]&0x03) + 1

	var parameterSets []byte
	pos := 6
	appendSets := func(count int) error {
		for i := 0; i < count; i++ {
			if pos+2 > len(codecPrivate) {
				return fmt.Errorf("h264: truncated avcC")
			}
			size := int(binary.BigEndian.Uint16(codecPrivate[pos:]))
			pos += 2
			if pos+size > len(codecPrivate) {
				return fmt.Errorf("h264: truncated avcC parameter set")
			}
			parameterSets = append(parameterSets, annexBStartCode...)
			parameterSets = append(parameterSets, codecPrivate[pos:pos+size]...)
			pos += size
		}
		return nil
	}

	if err := appendSets(int(codecPrivate[5] & 0x1f)); err != nil {
		return nil, err
	}
	if pos >= len(codecPrivate) {
		return nil, fmt.Errorf("h264: avcC has no PPS section")
	}
	numPPS := int(codecPrivate[pos])
	pos++
	if err := appendSets(numPPS); err != nil {
		return nil, err
	}

	return lengthPrefixedToAnnexB(nalLengthSize, parameterSets, isH264IDR), nil
}

func isH264IDR(header byte) bool {
	return header&0x1f == 5
}

// BLA, IDR or CRA.
func isH265IRAP(header byte) bool {
	t := (header >> 1) & 0x3f
	return t >= 16 && t <= 23
}

// configurationVersion is not checked, older muxers write 0.
func newHVCCToAnnexB(codecPrivate []byte) (frameRewriter, error) {
	if hasStartCode(codecPrivate) {
		return lengthPrefixedToAnnexB(4, codecPrivate, isH265IRAP), nil
	}
	if len(codecPrivate) < 23 {
		return nil, fmt.Errorf("h265: invalid or missing hvcC CodecPrivate (%d bytes, starts with % x)",
			len(codecPrivate), codecPrivate[:min(len(codecPrivate), 8)])
	}
	nalLengthSize := int(codecPrivate[21]&0x03) + 1
	numArrays := int(codecPrivate[22])

	var parameterSets []byte
	pos := 23
	for a := 0; a < numArrays; a++ {
		if pos+3 > len(codecPrivate) {
			return nil, fmt.Errorf("h265: truncated hvcC")
		}
		numNalus := int(binary.BigEndian.Uint16(codecPrivate[pos+1:]))
		pos += 3
		for n := 0; n < numNalus; n++ {
			if pos+2 > len(codecPrivate) {
				return nil, fmt.Errorf("h265: truncated hvcC")
			}
			size := int(binary.BigEndian.Uint16(codecPrivate[pos:]))
			pos += 2
			if pos+size > len(codecPrivate) {
				return nil, fmt.Errorf("h265: truncated hvcC parameter set")
			}
			parameterSets = append(parameterSets, annexBStartCode...)
			parameterSets = append(parameterSets, codecPrivate[pos:pos+size]...)
			pos += size
		}
	}

	return lengthPrefixedToAnnexB(nalLengthSize, parameterSets, isH265IRAP), nil
}

func hasStartCode(data []byte) bool {
	return bytes.HasPrefix(data, annexBStartCode) || bytes.HasPrefix(data, annexBStartCode[1:])
}

func lengthPrefixedToAnnexB(nalLengthSize int, parameterSets []byte,
	isRandomAccess func(header byte) bool) frameRewriter {
	return func(frame []byte, keyframe bool) []byte {
		// A 3-byte start code could also be a length prefix.
		if bytes.HasPrefix(frame, annexBStartCode) {
			if !keyframe {
				return frame
			}
			out := make([]byte, 0, len(parameterSets)+len(frame))
			return append(append(out, parameterSets...), frame...)
		}

		out := make([]byte, len(parameterSets), len(frame)+len(parameterSets)+16)
		copy(out, parameterSets)
		for i := 0; i+nalLengthSize <= len(frame); {
			var naluSize int
			for b := 0; b < nalLengthSize; b++ {
				naluSize = naluSize<<8 | int(frame[i+b])
			}
			i += nalLengthSize
			if naluSize <= 0 || i+naluSize > len(frame) {
				break
			}
			// Not every muxer sets the keyframe flag.
			if isRandomAccess(frame[i]) {
				keyframe = true
			}
			out = append(out, annexBStartCode...)
			out = append(out, frame[i:i+naluSize]...)
			i += naluSize
		}
		if !keyframe {
			return out[len(parameterSets):]
		}
		return out
	}
}

const blockTimecodeWrap = 65536 * time.Millisecond

// ebml-go reads the signed block offset as unsigned.
type timecodeFixer struct {
	highest time.Duration
	seen    bool
	fixed   int
}

func (f *timecodeFixer) fix(tc time.Duration) time.Duration {
	if f.seen && tc-f.highest > blockTimecodeWrap/2 {
		tc -= blockTimecodeWrap
		f.fixed++
	}
	if tc < 0 {
		tc = 0
	}
	if !f.seen || tc > f.highest {
		f.highest = tc
		f.seen = true
	}
	return tc
}

type streamPlan struct {
	videoTrackNumber uint
	videoCodecID     string
	videoSupport     videoSupport
	videoPayloader   rtp.Payloader
	videoRewrite     frameRewriter
	width, height    uint

	hasAudio         bool
	audioTrackNumber uint
	audioCodecID     string
	audioSkipReason  string
}

// cleanup drains the reader first, or the parser goroutine leaks or panics.
func openWebM(path string) (*webm.WebM, *webm.Reader, func(), error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, nil, nil, err
	}
	ctx := &webm.WebM{}
	reader, err := webm.Parse(file, ctx)
	if err != nil {
		file.Close()
		return nil, nil, nil, errors.New("not a WebM or Matroska file")
	}
	cleanup := func() {
		reader.Shutdown()
		for range reader.Chan {
		}
		file.Close()
	}
	return ctx, reader, cleanup, nil
}

func probe(videoFile string, disableAudio bool) (*streamPlan, error) {
	ctx, _, cleanup, err := openWebM(videoFile)
	if err != nil {
		return nil, fmt.Errorf("%w (remux it first, see --help)", err)
	}
	defer cleanup()

	videoTrack := ctx.FindFirstVideoTrack()
	if videoTrack == nil {
		return nil, fmt.Errorf("file contains no video track")
	}

	support, ok := supportedVideoCodecs[videoTrack.CodecID]
	if !ok {
		return nil, fmt.Errorf("video codec %q cannot be streamed "+
			"(supported: VP8, VP9, AV1, H264, H265)",
			videoTrack.CodecID)
	}
	payloader, rewrite, err := support.build(videoTrack.CodecPrivate)
	if err != nil {
		return nil, err
	}

	plan := &streamPlan{
		videoTrackNumber: videoTrack.TrackNumber,
		videoCodecID:     videoTrack.CodecID,
		videoSupport:     support,
		videoPayloader:   payloader,
		videoRewrite:     rewrite,
		width:            videoTrack.Video.PixelWidth,
		height:           videoTrack.Video.PixelHeight,
	}

	audioTrack := ctx.FindFirstAudioTrack()
	switch {
	case audioTrack == nil:
		plan.audioSkipReason = "file has no audio track"
	case disableAudio:
		plan.audioCodecID = audioTrack.CodecID
		plan.audioSkipReason = "audio disabled via --no-audio"
	case audioTrack.CodecID != codecIDOpus:
		plan.audioCodecID = audioTrack.CodecID
		plan.audioSkipReason = fmt.Sprintf(
			"audio codec %q cannot be sent (only Opus is supported), streaming video only",
			audioTrack.CodecID)
	default:
		plan.hasAudio = true
		plan.audioTrackNumber = audioTrack.TrackNumber
		plan.audioCodecID = audioTrack.CodecID
	}

	return plan, nil
}

type rtpSender struct {
	track      ghost.RTPWriter
	packetizer rtp.Packetizer
	clockRate  uint32
	offset     uint32
	prevTC     time.Duration
	prevSet    bool
}

func newRtpSender(track ghost.RTPWriter, payloader rtp.Payloader, clockRate uint32) *rtpSender {
	const rtpOutboundMTU uint16 = 1200
	return &rtpSender{
		track: track,
		packetizer: rtp.NewPacketizer(
			rtpOutboundMTU,
			0, // Value is handled when writing
			0, // Value is handled when writing
			payloader,
			rtp.NewRandomSequencer(),
			clockRate,
		),
		clockRate: clockRate,
	}
}

// Timestamps follow the container to keep audio and video in sync.
func (rs *rtpSender) send(data []byte, tc time.Duration) {
	want := uint32(int64(tc) * int64(rs.clockRate) / int64(time.Second))
	rs.packetizer.SkipSamples(want - rs.offset)
	rs.offset = want

	budget := defaultPacingBudget
	if rs.prevSet {
		if d := tc - rs.prevTC; d > 0 && d < maxPacingBudget {
			budget = d
		}
	}
	rs.prevTC = tc
	rs.prevSet = true

	if writeErrs := rs.writePaced(rs.packetizer.Packetize(data, 0), budget*4/5); writeErrs > 0 {
		log.Warn().Msgf("%d rtp write-errors occured", writeErrs)
	}
}

// Packets lost in a burst are never retransmitted.
func (rs *rtpSender) writePaced(packets []*rtp.Packet, budget time.Duration) int {
	writeErrs := 0
	write := func(p *rtp.Packet) {
		if err := rs.track.WriteRTP(p); err != nil {
			writeErrs++
		}
	}

	if len(packets) <= pacingThreshold || budget <= 0 {
		for _, p := range packets {
			write(p)
		}
		return writeErrs
	}

	slices := int(budget / pacingSlice)
	if slices < 1 {
		slices = 1
	}
	if slices > len(packets) {
		slices = len(packets)
	}
	perSlice := (len(packets) + slices - 1) / slices

	deadline := time.Now()
	for i := 0; i < len(packets); i += perSlice {
		end := i + perSlice
		if end > len(packets) {
			end = len(packets)
		}
		for _, p := range packets[i:end] {
			write(p)
		}
		if end < len(packets) {
			deadline = deadline.Add(pacingSlice)
			if d := time.Until(deadline); d > 0 {
				time.Sleep(d)
			}
		}
	}
	return writeErrs
}

type mediaFrame struct {
	data []byte
	tc   time.Duration
}

func clone(b []byte) []byte {
	out := make([]byte, len(b))
	copy(out, b)
	return out
}

func pump(sender *rtpSender, frames <-chan mediaFrame, started time.Time,
	tcOffset time.Duration, wg *sync.WaitGroup) {

	defer wg.Done()
	for frame := range frames {
		// now wait till it's ok to send that packet
		if wait := frame.tc - time.Since(started); wait > 0 {
			time.Sleep(wait)
		}
		sender.send(frame.data, tcOffset+frame.tc)
	}
}

func ingestControl(videoFile string, plan *streamPlan, videoTrack, audioTrack ghost.RTPWriter, loop bool) {
	videoSender := newRtpSender(videoTrack, plan.videoPayloader, videoClockRate)

	var audioSender *rtpSender
	if plan.hasAudio {
		audioSender = newRtpSender(audioTrack, &codecs.OpusPayloader{}, opusClockRate)
	}

	var tcOffset time.Duration
	for {
		duration, err := ingest(videoFile, plan, videoSender, audioSender, tcOffset)
		if err != nil {
			log.Warn().Err(err).Msg("Ingest video failed")
			return
		}
		if !loop {
			return
		}
		tcOffset += duration + 33*time.Millisecond
		log.Info().Msg("Restarting video playback")
	}
}

var bFrameWarning sync.Once

func ingest(videoFile string, plan *streamPlan, videoSender, audioSender *rtpSender,
	tcOffset time.Duration) (time.Duration, error) {

	_, reader, cleanup, err := openWebM(videoFile)
	if err != nil {
		return 0, err
	}
	defer cleanup()

	started := time.Now()
	var lastTC time.Duration
	var lastVideoTC time.Duration
	haveVideoTC := false
	fixer := &timecodeFixer{}

	// Separate pumps, so large video frames don't delay audio.
	videoCh := make(chan mediaFrame, videoQueueDepth)
	audioCh := make(chan mediaFrame, audioQueueDepth)
	var pumps sync.WaitGroup

	pumps.Add(1)
	go pump(videoSender, videoCh, started, tcOffset, &pumps)
	if audioSender != nil {
		pumps.Add(1)
		go pump(audioSender, audioCh, started, tcOffset, &pumps)
	}
	defer func() {
		close(videoCh)
		close(audioCh)
		pumps.Wait()
	}()

	for packet := range reader.Chan {
		if len(packet.Data) == 0 {
			// we're done
			break
		}
		if packet.Timecode == webm.BadTC {
			continue
		}
		if packet.TrackNumber != plan.videoTrackNumber &&
			(audioSender == nil || packet.TrackNumber != plan.audioTrackNumber) {
			continue
		}
		tc := fixer.fix(packet.Timecode)
		if tc > lastTC {
			lastTC = tc
		}

		if packet.TrackNumber != plan.videoTrackNumber {
			audioCh <- mediaFrame{data: clone(packet.Data), tc: tc}
			continue
		}

		// Decreasing timecodes mean B-frames.
		if haveVideoTC && tc < lastVideoTC {
			bFrameWarning.Do(func() {
				log.Warn().Msg("Video contains B-frames, which the eyeson media server does not " +
					"support. The picture will most likely stay black.")
			})
		}
		lastVideoTC = tc
		haveVideoTC = true

		// The parser reuses its buffers.
		data := packet.Data
		if plan.videoRewrite != nil {
			data = plan.videoRewrite(data, packet.Keyframe)
		} else {
			data = clone(data)
		}
		videoCh <- mediaFrame{data: data, tc: tc}
	}
	if fixer.fixed > 0 {
		log.Debug().Msgf("corrected %d wrapped block timecodes", fixer.fixed)
	}
	return lastTC, nil
}

// Get a room depending on the provided api-key or guestlink.
func getRoom(apiKeyOrGuestlink, apiEndpoint, user, roomID, userID, customCA string, insecure bool) (*eyeson.UserService, error) {
	clientOptions := []eyeson.ClientOption{}
	if len(customCA) > 0 {
		clientOptions = append(clientOptions, eyeson.WithCustomCAFile(customCA))
	}
	if insecure {
		clientOptions = append(clientOptions, eyeson.WithInsecureSkipVerify())
	}

	// determine if we have a guestlink
	if strings.HasPrefix(apiKeyOrGuestlink, "http") {
		// join as guest
		// guest-link: https://app.eyeson.team/?guest=h7IHRfwnV6Yuk3QtL2jbktuh
		u, err := url.Parse(apiKeyOrGuestlink)
		if err != nil {
			return nil, fmt.Errorf("Invalid guest-link")
		}
		params, err := url.ParseQuery(u.RawQuery)
		if err != nil {
			return nil, fmt.Errorf("Invalid guest-link")
		}
		guestToken, ok := params["guest"]
		if !ok || len(guestToken) != 1 {
			return nil, fmt.Errorf("Invalid guest-link")
		}
		client, err := eyeson.NewClient("", clientOptions...)
		if err != nil {
			return nil, err
		}
		baseURL, _ := url.Parse(apiEndpoint)
		client.BaseURL = baseURL
		return client.Rooms.GuestJoin(guestToken[0], userID, user, "")
	}

	// let's assume we have an apiKey, so fire up a new meeting
	client, err := eyeson.NewClient(apiKeyOrGuestlink, clientOptions...)
	if err != nil {
		return nil, err
	}
	baseURL, _ := url.Parse(apiEndpoint)
	client.BaseURL = baseURL
	options := map[string]string{}
	if len(userID) > 0 {
		options["user[id]"] = userID
	}
	if widescreenFlag {
		options["options[widescreen]"] = "true"
	}
	return client.Rooms.Join(roomID, user, options)
}

// room.WaitReady is silent for up to 180s.
func waitReady(room *eyeson.UserService) error {
	done := make(chan error, 1)
	go func() { done <- room.WaitReady() }()

	started := time.Now()
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case err := <-done:
			return err
		case <-ticker.C:
			log.Info().Msgf("Meeting is not ready yet, still waiting (%.0fs of 180s)...",
				time.Since(started).Seconds())
		}
	}
}

type callTerminator interface {
	TerminateCall() error
}

// TerminateCall blocks forever if the websocket is down.
func terminateCall(client callTerminator) {
	done := make(chan error, 1)
	go func() { done <- client.TerminateCall() }()

	select {
	case err := <-done:
		if err != nil {
			log.Warn().Err(err).Msg("Terminating the call failed")
		}
	case <-time.After(terminateTimeout):
		log.Warn().Msgf("Server did not confirm termination within %v, exiting anyway",
			terminateTimeout)
	}
}

func videoPlayerExample(apiKeyOrGuestlink, videoFile, apiEndpoint, user, roomID,
	userID string) {

	// The codec must be known before the ghost client is created.
	plan, err := probe(videoFile, noAudioFlag)
	if err != nil {
		log.Error().Err(err).Msg("Cannot play this file")
		return
	}
	log.Info().Msgf("Video: %s %dx%d", plan.videoCodecID, plan.width, plan.height)
	if plan.hasAudio {
		log.Info().Msgf("Audio: %s", plan.audioCodecID)
	} else {
		log.Warn().Msgf("Audio: %s", plan.audioSkipReason)
	}

	room, err := getRoom(apiKeyOrGuestlink, apiEndpoint, user, roomID, userID,
		customCAFileFlag, insecureSkipVerifyFlag)
	if err != nil {
		log.Error().Err(err).Msg("Failed to get room")
		return
	}
	log.Info().Msg("Joining meeting, waiting for it to become ready...")
	if err := waitReady(room); err != nil {
		log.Error().Err(err).Msg("The meeting never became ready")
		log.Error().Msg("A guest link only joins a meeting that is already running, and " +
			"eyeson shuts a meeting down once the last participant leaves. Open the meeting " +
			"in a browser and stay joined, or pass an API key instead to create a new one.")
		return
	}

	log.Info().Msgf("Guest-link: %s", room.Data.Links.GuestJoin)
	log.Info().Msgf("GUI-link: %s", room.Data.Links.Gui)

	clientOptions := []ghost.ClientOption{
		ghost.WithCustomLogger(&Logger{}),
		ghost.WithSendOnly(),
		plan.videoSupport.ghostOption,
	}
	if len(customCAFileFlag) > 0 {
		clientOptions = append(clientOptions, ghost.WithCustomCAFile(customCAFileFlag))
	}
	if insecureSkipVerifyFlag {
		clientOptions = append(clientOptions, ghost.WithInsecureSkipVerify())
	}

	eyesonClient, err := ghost.NewClient(room.Data, clientOptions...)
	if err != nil {
		log.Error().Err(err).Msg("Failed to create eyeson-client")
		return
	}
	defer eyesonClient.Destroy()

	eyesonClient.SetTerminatedHandler(func() {
		log.Info().Msg("Call terminated")
		os.Exit(0)
	})

	if verboseFlag {
		eyesonClient.SetDataChannelHandler(func(data []byte) {
			log.Debug().Msgf("DC message: %s", string(data))
		})
	}

	playbackTerminatedCh := make(chan bool)
	eyesonClient.SetConnectedHandler(func(connected bool, localVideoTrack ghost.RTPWriter,
		localAudioTrack ghost.RTPWriter) {
		if !connected {
			return
		}
		go func() {
			time.Sleep(startDelayFlag)
			ingestControl(videoFile, plan, localVideoTrack, localAudioTrack, loopFlag)
		}()
	})

	if err := eyesonClient.Call(); err != nil {
		log.Error().Err(err).Msg("Failed to call")
		return
	}

	// install signal-handler
	chStop := make(chan os.Signal, 1)
	signal.Notify(chStop, syscall.SIGINT, syscall.SIGTERM)

	// Block until playback is done
	select {
	case <-chStop:
	case <-playbackTerminatedCh:
	}

	// Lets a second Ctrl-C force quit.
	signal.Stop(chStop)

	log.Info().Msg("Stopping. So terminating this call (Ctrl-C again to force)")
	// terminate this call
	terminateCall(eyesonClient)
}

type Logger struct{}

// Error log error msg
func (sl *Logger) Error(format string, v ...interface{}) {
	log.Error().Msgf(format, v...)
}

// Warn log warn message
func (sl *Logger) Warn(format string, v ...interface{}) {
	log.Warn().Msgf(format, v...)
}

// Info log info message
func (sl *Logger) Info(format string, v ...interface{}) {
	log.Info().Msgf(format, v...)
}

// Debug log debug message
func (sl *Logger) Debug(format string, v ...interface{}) {
	log.Debug().Msgf(format, v...)
}

// Trace log trace message
func (sl *Logger) Trace(format string, v ...interface{}) {
	log.Trace().Msgf(format, v...)
}

func initLogging() {
	switch {
	case verboseFlag:
		zerolog.SetGlobalLevel(zerolog.DebugLevel)
	case traceFlag:
		zerolog.SetGlobalLevel(zerolog.TraceLevel)
	case quietFlag:
		zerolog.SetGlobalLevel(zerolog.Disabled)
	default:
		zerolog.SetGlobalLevel(zerolog.InfoLevel)
	}
}

// The webm parser logs every discardable block.
func silenceLibraryLogging() {
	stdlog.SetOutput(io.Discard)
}

func main() {
	silenceLibraryLogging()

	log.Logger = log.Output(
		zerolog.ConsoleWriter{Out: os.Stderr, TimeFormat: "15:04:05.000"})
	zerolog.TimeFieldFormat = zerolog.TimeFormatUnixMs
	zerolog.SetGlobalLevel(zerolog.InfoLevel)
	cobra.OnInitialize(initLogging)

	rootCommand.Version = Version
	rootCommand.SetVersionTemplate("{{.Version}}\n")
	rootCommand.Long = `ghost-player streams a local video file into an eyeson meeting.

Input must be a WebM or Matroska file. Video can be VP8, VP9, AV1, H264 or H265;
audio must be Opus. If the audio is in another format the video still plays,
without sound.`

	rootCommand.Flags().StringVarP(&apiEndpointFlag, "api-endpoint", "", "https://api.eyeson.team", "Set api-endpoint")
	rootCommand.Flags().StringVarP(&userFlag, "user", "", "ghost-player", "User name to use")
	rootCommand.Flags().StringVarP(&userIDFlag, "user-id", "", "", "User id to use")
	rootCommand.Flags().StringVarP(&roomIDFlag, "room-id", "", "", "Room ID. If left empty, a new meeting will be created on each request")
	rootCommand.Flags().BoolVarP(&verboseFlag, "verbose", "v", false, "verbose output")
	rootCommand.Flags().BoolVarP(&traceFlag, "trace", "", false, "trace output")
	rootCommand.Flags().BoolVarP(&quietFlag, "quiet", "q", false, "no logging output")
	rootCommand.Flags().StringVarP(&customCAFileFlag, "custom-ca", "", "", "custom CA file")
	rootCommand.Flags().BoolVarP(&widescreenFlag, "widescreen", "", true, "start room in widescreen mode")
	rootCommand.Flags().BoolVarP(&loopFlag, "loop", "", true, "Restart video-playback on EOF")
	rootCommand.Flags().BoolVarP(&insecureSkipVerifyFlag, "insecure", "", false, "if true don't verify remote tls certificates")
	// Packets sent before DTLS is up are lost, the first keyframe with them.
	rootCommand.Flags().DurationVarP(&startDelayFlag, "start-delay", "", 2*time.Second, "wait this long after connecting before playback starts, so the first keyframe is not lost")
	rootCommand.Flags().BoolVarP(&noAudioFlag, "no-audio", "", false, "never send audio, even if the codec would match")

	rootCommand.Execute()
}