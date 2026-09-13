package main

import (
	"encoding/binary"
	"fmt"
	"net/url"
	"os"
	"os/signal"
	"strings"
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
	checkFlag              bool

	rootCommand = &cobra.Command{
		Use:   "ghost-player [flags] $API_KEY|$GUEST_LINK VIDEO_FILE\n  ghost-player --check VIDEO_FILE",
		Short: "ghost-player",
		Args:  cobra.RangeArgs(1, 2),
		Run: func(cmd *cobra.Command, args []string) {
			if checkFlag {
				// Inspect only: no API call, no meeting, no connection.
				os.Exit(checkFile(args[len(args)-1]))
			}
			if len(args) < 2 {
				fmt.Fprintln(os.Stderr,
					"need an API key or guest link and a video file, or --check with just a video file")
				cmd.Usage()
				os.Exit(2)
			}
			videoPlayerExample(args[0], args[1], apiEndpointFlag,
				userFlag, roomIDFlag, userIDFlag)
		},
	}
)

// ---------------------------------------------------------------------------
// Codec support
// ---------------------------------------------------------------------------

// Matroska/WebM CodecID strings. See https://www.matroska.org/technical/codec_specs.html
const (
	codecIDVP8    = "V_VP8"
	codecIDVP9    = "V_VP9"
	codecIDAV1    = "V_AV1"
	codecIDH264   = "V_MPEG4/ISO/AVC"
	codecIDH265   = "V_MPEGH/ISO/HEVC"
	codecIDOpus   = "A_OPUS"
	codecIDVorbis = "A_VORBIS"
	codecIDAAC    = "A_AAC"
)

const (
	videoClockRate = 90000
	opusClockRate  = 48000

	// Frames of at most this many packets are written in one go; pacing them
	// would add latency without easing any real burst.
	pacingThreshold = 8
	// Granularity of the pacer. Sleeps much shorter than this are not
	// reliable on either Linux or macOS.
	pacingSlice         = time.Millisecond
	defaultPacingBudget = 20 * time.Millisecond
	maxPacingBudget     = 100 * time.Millisecond

	// How long to wait for the server to confirm call termination before
	// giving up and exiting anyway.
	terminateTimeout = 5 * time.Second
)

// frameRewriter adapts a container frame to what the RTP payloader expects.
// nil means "pass the frame through untouched".
type frameRewriter func(frame []byte, keyframe bool) []byte

// videoSupport describes how one container video codec is mapped onto the
// eyeson/ghost webrtc stack.
type videoSupport struct {
	// ghostOption tells the ghost client which codec to negotiate in SDP.
	ghostOption ghost.ClientOption
	// build returns the RTP payloader plus an optional frame rewriter.
	// codecPrivate is the Matroska CodecPrivate blob (avcC for H264, nil for others).
	build func(codecPrivate []byte) (rtp.Payloader, frameRewriter, error)
}

// supportedVideoCodecs maps a container CodecID to the eyeson media server
// codecs. The eyeson media server accepts VP8, VP9, AV1 and H264.
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
}

// ---------------------------------------------------------------------------
// AV1: split temporal units into OBUs
//
// Matroska stores one whole AV1 temporal unit (temporal delimiter + optional
// sequence header + frame OBUs) per block, but pion's AV1Payloader expects a
// single OBU per call. Handed a full temporal unit it mistakes the entire unit
// for a sequence header, caches it, and then panics on the following frame
// with a slice-bounds error. Splitting the unit first fixes both problems and
// lets pion's sequence-header caching work as intended.
// ---------------------------------------------------------------------------

const (
	obuTypeTemporalDelimiter = 2
	obuTypePadding           = 15

	obuExtensionFlagMask = 0x04
	obuHasSizeFieldMask  = 0x02
	obuTypeMask          = 0x78
	obuTypeShift         = 3
)

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

// splitOBUs walks the low-overhead bitstream format and returns the OBUs that
// are allowed on the wire. Temporal delimiters and padding are dropped, as
// required by the AV1 RTP spec.
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
			// No size field: this OBU runs to the end of the buffer.
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
			// dropped
		default:
			obus = append(obus, temporalUnit[i:end])
		}
		i = end
	}
	return obus
}

// ---------------------------------------------------------------------------
// H264: avcC (length prefixed NALUs) -> Annex-B (start code prefixed NALUs)
//
// Matroska stores H264 in the same "AVCC" form as MP4: every NAL unit is
// prefixed with a 1-4 byte big endian length instead of a start code. pion's
// H264Payloader expects an Annex-B stream, and it picks up SPS/PPS from that
// stream to build STAP-A packets, so SPS/PPS from CodecPrivate must be
// injected in front of every keyframe.
// ---------------------------------------------------------------------------

var annexBStartCode = []byte{0x00, 0x00, 0x00, 0x01}

func newAVCCToAnnexB(codecPrivate []byte) (frameRewriter, error) {
	if len(codecPrivate) < 7 || codecPrivate[0] != 1 {
		return nil, fmt.Errorf("h264: invalid or missing avcC CodecPrivate (%d bytes)", len(codecPrivate))
	}
	nalLengthSize := int(codecPrivate[4]&0x03) + 1

	var parameterSets []byte
	// byte 5 holds numOfSequenceParameterSets in its low 5 bits, the
	// length-prefixed sets themselves start at byte 6.
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

	return func(frame []byte, keyframe bool) []byte {
		out := make([]byte, 0, len(frame)+len(parameterSets)+16)
		if keyframe {
			out = append(out, parameterSets...)
		}
		for i := 0; i+nalLengthSize <= len(frame); {
			var naluSize int
			for b := 0; b < nalLengthSize; b++ {
				naluSize = naluSize<<8 | int(frame[i+b])
			}
			i += nalLengthSize
			if naluSize <= 0 || i+naluSize > len(frame) {
				break
			}
			out = append(out, annexBStartCode...)
			out = append(out, frame[i:i+naluSize]...)
			i += naluSize
		}
		return out
	}, nil
}

// ---------------------------------------------------------------------------
// Probing
// ---------------------------------------------------------------------------

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

// openWebM parses the container header and returns the parsed context, the
// packet reader and a cleanup function.
//
// Careful: webm.Parse starts a background goroutine that keeps reading from
// the file, and at EOF that goroutine parks waiting for a seek command. The
// file must therefore not be closed until the reader has been shut down and
// its channel drained - otherwise the goroutine either leaks or panics on a
// closed file.
func openWebM(path string) (*webm.WebM, *webm.Reader, func(), error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, nil, nil, err
	}
	ctx := &webm.WebM{}
	reader, err := webm.Parse(file, ctx)
	if err != nil {
		file.Close()
		return nil, nil, nil, fmt.Errorf("not a readable webm/matroska file: %w", err)
	}
	cleanup := func() {
		reader.Shutdown()
		for range reader.Chan { // drain so the goroutine can finish
		}
		file.Close()
	}
	return ctx, reader, cleanup, nil
}

// probe opens the file, reads the container header and decides what can be
// streamed. It deliberately runs before the meeting is joined, because the
// video codec has to be known when the ghost client is created.
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
		return nil, fmt.Errorf("video codec %q is not supported by the eyeson media server "+
			"(supported: VP8, VP9, AV1, H264) - transcode the file first",
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

// ---------------------------------------------------------------------------
// RTP sending
// ---------------------------------------------------------------------------

type rtpSender struct {
	track      ghost.RTPWriter
	packetizer rtp.Packetizer
	clockRate  uint32
	// offset mirrors how far the packetizer's timestamp has been advanced from
	// its random starting value.
	offset  uint32
	prevTC  time.Duration
	prevSet bool
}

func newRtpSender(track ghost.RTPWriter, payloader rtp.Payloader, clockRate uint32) *rtpSender {
	const rtpOutboundMTU uint16 = 1200
	return &rtpSender{
		track: track,
		packetizer: rtp.NewPacketizer(
			rtpOutboundMTU,
			0, // payload type is handled when writing
			0, // ssrc is handled when writing
			payloader,
			rtp.NewRandomSequencer(),
			clockRate,
		),
		clockRate: clockRate,
	}
}

// send packetizes one container frame and writes it to the track.
//
// The RTP timestamp is derived from the container timecode rather than being
// advanced by a fixed amount per frame. That matters twice over: it keeps
// audio and video in sync, and it survives H264 B-frames, whose timecodes
// legitimately jump backwards. Timestamps are computed as an absolute offset
// from the packetizer's random start value, so a backwards jump produces a
// uint32 that wraps around to exactly the right value.
func (rs *rtpSender) send(data []byte, tc time.Duration) {
	want := uint32(int64(tc) * int64(rs.clockRate) / int64(time.Second))
	rs.packetizer.SkipSamples(want - rs.offset)
	rs.offset = want

	// Spread this frame's packets over roughly the time until the next frame.
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

// writePaced releases a frame's packets in small time slices instead of
// dumping them into the socket all at once.
//
// A high-bitrate source produces enormous frames: a 25 Mbit/s 720p file yields
// well over a hundred RTP packets per frame, and writing them back to back
// bursts far above any sane send rate. Because the ghost client registers no
// NACK responder, a packet lost to that burst is never retransmitted, and a
// damaged keyframe means the receiver shows nothing until the next keyframe
// happens to arrive intact.
//
// The budget is bounded by the frame interval, so pacing never makes playback
// fall behind: whatever time is spent here is time the ingest loop would have
// spent sleeping anyway.
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

// ---------------------------------------------------------------------------
// Ingest
// ---------------------------------------------------------------------------

func ingestControl(videoFile string, plan *streamPlan, videoTrack, audioTrack ghost.RTPWriter, loop bool) {
	videoSender := newRtpSender(videoTrack, plan.videoPayloader, videoClockRate)

	var audioSender *rtpSender
	if plan.hasAudio {
		audioSender = newRtpSender(audioTrack, &codecs.OpusPayloader{}, opusClockRate)
	}

	// Timecodes restart at zero on every pass, so keep an offset to make RTP
	// timestamps monotonic across loops.
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

func ingest(videoFile string, plan *streamPlan, videoSender, audioSender *rtpSender,
	tcOffset time.Duration) (time.Duration, error) {

	_, reader, cleanup, err := openWebM(videoFile)
	if err != nil {
		return 0, err
	}
	defer cleanup()

	started := time.Now()
	var lastTC time.Duration

	for packet := range reader.Chan {
		if len(packet.Data) == 0 {
			// end of file
			return lastTC, nil
		}
		if packet.Timecode == webm.BadTC {
			continue
		}
		if packet.TrackNumber != plan.videoTrackNumber &&
			(audioSender == nil || packet.TrackNumber != plan.audioTrackNumber) {
			continue
		}

		// Pace against the wall clock. Both tracks share one clock, which is
		// what keeps them in sync relative to each other.
		if wait := packet.Timecode - time.Since(started); wait > 0 {
			time.Sleep(wait)
		}
		lastTC = packet.Timecode

		if packet.TrackNumber == plan.videoTrackNumber {
			data := packet.Data
			if plan.videoRewrite != nil {
				data = plan.videoRewrite(data, packet.Keyframe)
			}
			videoSender.send(data, tcOffset+packet.Timecode)
			continue
		}
		audioSender.send(packet.Data, tcOffset+packet.Timecode)
	}
	return lastTC, nil
}

// ---------------------------------------------------------------------------
// Meeting setup
// ---------------------------------------------------------------------------

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

// waitReady wraps room.WaitReady with progress output. The underlying call
// polls the API silently for up to 180 seconds, which looks indistinguishable
// from a hang when the meeting behind a guest link is no longer running.
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

// callTerminator is the slice of the ghost client used during shutdown, kept
// as an interface so the timeout behaviour can be tested.
type callTerminator interface {
	TerminateCall() error
}

// terminateCall asks the server to end the call without waiting forever.
//
// Client.TerminateCall sends a terminate message over the signalling websocket
// and then blocks on context.Background() until the server confirms. A nil
// Done channel never fires, so if the websocket is down - in which case the
// message is dropped by the sender goroutine anyway - the call never returns
// and the process cannot be shut down cleanly.
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

	// Probe first: the negotiated video codec depends on the file, so this has
	// to happen before the ghost client is created.
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
		go ingestControl(videoFile, plan, localVideoTrack, localAudioTrack, loopFlag)
	})

	if err := eyesonClient.Call(); err != nil {
		log.Error().Err(err).Msg("Failed to call")
		return
	}

	chStop := make(chan os.Signal, 1)
	signal.Notify(chStop, syscall.SIGINT, syscall.SIGTERM)

	select {
	case <-chStop:
	case <-playbackTerminatedCh:
	}

	// Restore default signal handling before starting shutdown. While
	// signal.Notify is active the runtime delivers SIGINT to the channel above
	// instead of killing the process, so a second Ctrl-C would be swallowed and
	// a shutdown that hangs could only be escaped with kill -9.
	signal.Stop(chStop)

	log.Info().Msg("Stopping. So terminating this call (Ctrl-C again to force)")
	terminateCall(eyesonClient)
}

// ---------------------------------------------------------------------------
// Logging / cli boilerplate (unchanged)
// ---------------------------------------------------------------------------

type Logger struct{}

func (sl *Logger) Error(format string, v ...interface{}) { log.Error().Msgf(format, v...) }
func (sl *Logger) Warn(format string, v ...interface{})  { log.Warn().Msgf(format, v...) }
func (sl *Logger) Info(format string, v ...interface{})  { log.Info().Msgf(format, v...) }
func (sl *Logger) Debug(format string, v ...interface{}) { log.Debug().Msgf(format, v...) }
func (sl *Logger) Trace(format string, v ...interface{}) { log.Trace().Msgf(format, v...) }

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

func main() {
	log.Logger = log.Output(
		zerolog.ConsoleWriter{Out: os.Stderr, TimeFormat: "15:04:05.000"})
	zerolog.TimeFieldFormat = zerolog.TimeFormatUnixMs
	zerolog.SetGlobalLevel(zerolog.InfoLevel)
	cobra.OnInitialize(initLogging)

	rootCommand.Version = Version
	rootCommand.SetVersionTemplate(`{{.Version}}`)
	rootCommand.Long = `ghost-player streams a local video file into an eyeson meeting.

Input must be a WebM/Matroska file whose codecs the eyeson media server
accepts: VP8, VP9, AV1 or H264 video, and Opus audio. Nothing is transcoded,
frames are passed through as they are stored in the file.

If the audio codec does not match, playback continues without audio.

To prepare an arbitrary file, remux (fast, no quality loss) with:
  ffmpeg -i input.mp4 -c:v copy -c:a libopus out.mkv
or transcode the video as well if its codec is not in the list above:
  ffmpeg -i input.mov -c:v libvpx-vp9 -c:a libopus out.webm`

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
	rootCommand.Flags().BoolVarP(&noAudioFlag, "no-audio", "", false, "never send audio, even if the codec would match")
	rootCommand.Flags().BoolVarP(&checkFlag, "check", "", false, "inspect the video file and report whether it can be streamed, then exit without connecting")

	rootCommand.Execute()
}
