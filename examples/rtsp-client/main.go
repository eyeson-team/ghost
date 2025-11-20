package main

import (
	"fmt"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"

	standardLog "log"

	"github.com/rs/zerolog"
	log "github.com/rs/zerolog/log"

	"github.com/bluenviron/gortsplib/v4"
	"github.com/bluenviron/gortsplib/v4/pkg/base"
	"github.com/bluenviron/gortsplib/v4/pkg/description"
	"github.com/bluenviron/gortsplib/v4/pkg/format"
	"github.com/bluenviron/gortsplib/v4/pkg/format/rtph264"
	"github.com/bluenviron/gortsplib/v4/pkg/format/rtph265"

	rtsph264 "github.com/bluenviron/mediacommon/pkg/codecs/h264"
	rtsph265 "github.com/bluenviron/mediacommon/pkg/codecs/h265"
	"github.com/eyeson-team/eyeson-go"
	ghost "github.com/eyeson-team/ghost/v2"
	"github.com/pion/rtp"
	"github.com/spf13/cobra"
)

var (
	apiEndpointFlag        string
	userFlag               string
	userIDFlag             string
	roomIDFlag             string
	verboseFlag            bool
	widescreenFlag         bool
	passThroughFlag        bool
	traceFlag              bool
	quietFlag              bool
	customCAFileFlag       string
	insecureSkipVerifyFlag bool
	useH265CodecFlag       bool

	rootCommand = &cobra.Command{
		Use:   "rtsp-client [flags] $API_KEY|$GUEST_LINK RTSP_CONNECT_URL",
		Short: "rtsp-client",
		Args:  cobra.MinimumNArgs(2),
		PreRun: func(cmd *cobra.Command, args []string) {

		},
		Run: func(cmd *cobra.Command, args []string) {
			rtspClientExample(args[0], args[1], apiEndpointFlag,
				userFlag, roomIDFlag, userIDFlag)
		},
	}
)

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

// customWriter is an io.Writer that uses zerolog's logger.
type customLogWriter struct{}

func (cw customLogWriter) Write(p []byte) (n int, err error) {
	// Use zerolog to log standard log messages
	log.Debug().Msg(string(p))
	return len(p), nil
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
	standardLog.SetOutput(customLogWriter{})
}

func main() {
	log.Logger = log.Output(
		zerolog.ConsoleWriter{
			Out: os.Stderr, TimeFormat: "15:04:05.000"})
	zerolog.TimeFieldFormat = zerolog.TimeFormatUnixMs
	zerolog.SetGlobalLevel(zerolog.InfoLevel)
	cobra.OnInitialize(initLogging)

	rootCommand.Flags().StringVarP(&apiEndpointFlag, "api-endpoint", "", "https://api.eyeson.team", "Set api-endpoint")
	rootCommand.Flags().StringVarP(&userFlag, "user", "", "rtsp-test", "User name to use")
	rootCommand.Flags().StringVarP(&userIDFlag, "user-id", "", "", "User id to use")
	rootCommand.Flags().StringVarP(&roomIDFlag, "room-id", "", "", "Room ID. If left empty, a new meeting will be created on each request")
	rootCommand.Flags().BoolVarP(&verboseFlag, "verbose", "v", false, "verbose output")
	rootCommand.Flags().BoolVarP(&traceFlag, "trace", "", false, "trace output")
	rootCommand.Flags().BoolVarP(&quietFlag, "quiet", "q", false, "no logging output")
	rootCommand.Flags().BoolVarP(&widescreenFlag, "widescreen", "", true, "start room in widescreen mode")
	rootCommand.Flags().BoolVarP(&passThroughFlag, "passthrough", "", false, "if true just passthrough all H264 NAL-Units")
	rootCommand.Flags().StringVarP(&customCAFileFlag, "custom-ca", "", "", "custom CA file")
	rootCommand.Flags().BoolVarP(&insecureSkipVerifyFlag, "insecure", "", false, "if true don't verify remote tls certificates")
	rootCommand.Flags().BoolVarP(&useH265CodecFlag, "h265", "", false, "If true, expect h265 instead of h264")

	rootCommand.Execute()
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

func rtspClientExample(apiKeyOrGuestlink, rtspConnectURL, apiEndpoint, user,
	roomID, userID string) {

	room, err := getRoom(apiKeyOrGuestlink, apiEndpoint, user, roomID, userID, customCAFileFlag,
		insecureSkipVerifyFlag)
	if err != nil {
		log.Error().Err(err).Msg("Failed to get room")
		return
	}
	log.Debug().Msg("Waiting for room to become ready")
	err = room.WaitReady()
	if err != nil {
		log.Fatal().Err(err).Msg("Failed")
	}

	log.Info().Msgf("Guest-link: %s", room.Data.Links.GuestJoin)
	log.Info().Msgf("GUI-link: %s", room.Data.Links.Gui)

	clientOptions := []ghost.ClientOption{
		ghost.WithCustomLogger(&Logger{}),
		ghost.WithSendOnly(),
	}
	if len(customCAFileFlag) > 0 {
		clientOptions = append(clientOptions, ghost.WithCustomCAFile(customCAFileFlag))
	}

	if insecureSkipVerifyFlag {
		clientOptions = append(clientOptions, ghost.WithInsecureSkipVerify())
	}

	if useH265CodecFlag {
		clientOptions = append(clientOptions, ghost.WithForceH265Codec())
	} else {
		clientOptions = append(clientOptions, ghost.WithForceH264Codec())
	}

	eyesonClient, err := ghost.NewClient(room.Data, clientOptions...)
	if err != nil {
		log.Error().Err(err).Msg("Failed to create eyeson-client")
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

	rtspTerminatedCh := make(chan bool)
	eyesonClient.SetConnectedHandler(func(connected bool, localVideoTrack ghost.RTPWriter,
		localAudioTrack ghost.RTPWriter) {
		log.Info().Msgf("Webrtc connected. Connecting to %s", rtspConnectURL)
		setupRtspClient(localVideoTrack, rtspConnectURL, rtspTerminatedCh, useH265CodecFlag)
	})

	if err := eyesonClient.Call(); err != nil {
		log.Error().Err(err).Msg("Failed to call")
		return
	}

	// install signal-handler
	chStop := make(chan os.Signal, 1)
	signal.Notify(chStop, syscall.SIGINT, syscall.SIGTERM)

	// Block until rtsp-connection is done
	select {
	case <-chStop:
		break
	case <-rtspTerminatedCh:
		break
	}

	log.Info().Msg("RTSP connection is done. So terminating this call")
	// terminate this call
	eyesonClient.TerminateCall()
}

func forwardh265(nalus [][]byte, encoder *rtph265.Encoder, videoTrack ghost.RTPWriter, rtpTimestamp uint32) error {
	pkts, err := encoder.Encode(nalus)
	if err != nil {
		return fmt.Errorf("error while encoding H265: %v", err)
	}

	for _, pkt := range pkts {
		// log.Debug().Msgf("Final ts: %d seq: %d len: %d mark: %v", pkt.Timestamp, pkt.SequenceNumber,
		// 	len(pkt.Payload), pkt.Marker)
		pkt.Timestamp = rtpTimestamp
		err = videoTrack.WriteRTP(pkt)
		if err != nil {
			return fmt.Errorf("Failed to write h265 sample %v", err)
		}
	}
	return nil
}

func forwardh264(nalus [][]byte, encoder *rtph264.Encoder, videoTrack ghost.RTPWriter, rtpTimestamp uint32) error {
	pkts, err := encoder.Encode(nalus)
	if err != nil {
		return fmt.Errorf("error while encoding H264: %v", err)
	}

	for _, pkt := range pkts {
		//log.Debug().Msgf("Final ts: %d seq: %d len: %d mark: %v", pkt.Timestamp, pkt.SequenceNumber,
		//	len(pkt.Payload), pkt.Marker)
		pkt.Timestamp = rtpTimestamp
		err = videoTrack.WriteRTP(pkt)
		if err != nil {
			return fmt.Errorf("Failed to write h264 sample %v", err)
		}
	}
	return nil
}

func setupRtspClient(videoTrack ghost.RTPWriter, rtspConnectURL string,
	rtspTerminated chan<- bool, codecH265 bool) {
	go func() {

		// Make ReadBufferSize large enough to fit whole keyframes.
		// Some IP-Cams rely on ip-fragmentation which results
		// in large UDP packets.
		c := gortsplib.Client{} //ReadBufferSize: 2 << 20}
		// parse URL
		u, err := base.ParseURL(rtspConnectURL)
		if err != nil {
			log.Error().Err(err).Msg("Parse RTSP-Url failed")
			rtspTerminated <- true
			return
		}

		// connect to the server
		err = c.Start(u.Scheme, u.Host)
		if err != nil {
			log.Error().Err(err).Msg("Connecting to rtsp server failed")
			rtspTerminated <- true
			return
		}
		defer c.Close()

		// find published tracks
		session, baseURL, err := c.Describe(u)
		if err != nil {
			log.Error().Err(err).Msg("Connecting to rtsp server failed")
			rtspTerminated <- true
			return
		}

		log.Debug().Msgf("baseurl: %s", baseURL)

		var fh265 *format.H265
		mediaH265 := session.FindFormat(&fh265)

		var fh264 *format.H264
		mediaH264 := session.FindFormat(&fh264)

		if codecH265 && mediaH265 == nil {
			log.Error().Msg("Expecting h265 codec but no h265 media found")
			if mediaH264 != nil {
				log.Error().Msg("Since h264-codec is present, think of starting without --h265 switch")
			}
			rtspTerminated <- true
			return
		}

		if !codecH265 && mediaH264 == nil {
			log.Error().Msg("Expecting h264 codec but no h264 media found")
			if mediaH265 != nil {
				log.Error().Msg("Since h265-codec is present, think of starting with --h265 switch")
			}
			rtspTerminated <- true
			return
		}

		if codecH265 {
			setupRtspClientH265(videoTrack, rtspTerminated, fh265, mediaH265, session, &c)
		} else {
			setupRtspClientH264(videoTrack, rtspTerminated, fh264, mediaH264, session, &c)
		}

	}()
}

func setupRtspClientH265(videoTrack ghost.RTPWriter,
	rtspTerminated chan<- bool, fh265 *format.H265, mediaH265 *description.Media,
	session *description.Session, c *gortsplib.Client) {

	sps := fh265.SPS
	pps := fh265.PPS
	vps := fh265.VPS

	if !passThroughFlag {
		if sps == nil || pps == nil || vps == nil {
			if sps == nil {
				log.Warn().Msg("SPS not present")
			}
			if pps == nil {
				log.Warn().Msg("PPS not present")
			}
			if vps == nil {
				log.Warn().Msg("VPS not present")
			}
		}
	}

	h265Encoder := rtph265.Encoder{
		PayloadType:    96,
		PayloadMaxSize: 1200,
	}
	h265Encoder.Init()

	// setup RTP->H265 decoder
	rtpDec := &rtph265.Decoder{}
	rtpDec.Init()

	// setup a single media
	_, err := c.Setup(session.BaseURL, mediaH265, 0, 0)
	if err != nil {
		log.Error().Err(err).Msg("Failed to start rtsp")
		rtspTerminated <- true
		return
	}

	var lastRTPts uint32

	firstKeyFrame := false

	// buffers nalus for the same timestamp (rtpTimestamp)
	nalusBuffer := [][]byte{}

	onRTPPacket := func(pkt *rtp.Packet) {
		//log.Info().Msgf("packet %d", pkt.Timestamp)

		if lastRTPts != 0 {
			if pkt.Timestamp < lastRTPts {
				log.Info().Msg("Warning: Non monotonic ts. Probably a B-Frame, but B-frames not supported.")
			}
		}

		if (firstKeyFrame || passThroughFlag) && lastRTPts != pkt.Timestamp && len(nalusBuffer) > 0 {
			// last frame is complete, so forward nalus and clear the buffer
			if err := forwardh265(nalusBuffer, &h265Encoder, videoTrack, lastRTPts); err != nil {
				log.Error().Err(err).Msg("Failed to forward")
				return
			}
			nalusBuffer = [][]byte{}
		}

		// decode H265 NALUs from the RTP packet
		nalus, err := rtpDec.Decode(pkt)
		if err != nil {
			if err != rtph265.ErrMorePacketsNeeded {
				log.Warn().Msgf("Decode failed: %s", err)
			}
			return
		}

		// append to our buffer
		// This buffer contains all nalus for a timestamp
		nalusBuffer = append(nalusBuffer, nalus...)

		if !passThroughFlag {
			if !firstKeyFrame {
				firstKeyFrame = containsH265KeyFrame(nalusBuffer)
			}
		}

		lastRTPts = pkt.Timestamp
	}

	c.OnPacketRTP(mediaH265, fh265, onRTPPacket)

	_, err = c.Play(nil)
	if err != nil {
		log.Error().Err(err).Msg("Failed to start rtsp")
		rtspTerminated <- true
		return
	}

	// wait until a fatal error
	if err := c.Wait(); err != nil {
		log.Error().Err(err).Msg("RTSP finished with err")
	}

	rtspTerminated <- true

}

func containsH265KeyFrame(nalus [][]byte) bool {
	for _, nalu := range nalus {
		typ := rtsph265.NALUType((nalu[0] >> 1) & 0x3F)
		switch typ {
		case rtsph265.NALUType_IDR_N_LP, rtsph265.NALUType_IDR_W_RADL:
			return true
		}
	}
	return false
}

func containsH264SEI(nalus [][]byte) bool {
	for _, nalu := range nalus {
		typ := rtsph264.NALUType(nalu[0] & 0x1F)
		switch typ {
		case rtsph264.NALUTypeSEI:
			return true
		}
	}
	return false
}

func containsH264PPS(nalus [][]byte) bool {
	for _, nalu := range nalus {
		typ := rtsph264.NALUType(nalu[0] & 0x1F)
		switch typ {
		case rtsph264.NALUTypePPS:
			return true
		}
	}
	return false
}

func containsH264KeyFrame(nalus [][]byte) bool {
	for _, nalu := range nalus {
		typ := rtsph264.NALUType(nalu[0] & 0x1F)
		switch typ {
		case rtsph264.NALUTypeIDR:
			return true
		}
	}
	return false
}

func debugH264NALUs(nalus [][]byte) {
	for _, nalu := range nalus {
		typ := rtsph264.NALUType(nalu[0] & 0x1F)
		log.Trace().Msgf("type: %v", typ)
	}
}

func setupRtspClientH264(videoTrack ghost.RTPWriter,
	rtspTerminated chan<- bool, fh264 *format.H264, mediaH264 *description.Media,
	session *description.Session, c *gortsplib.Client) {

	sps := fh264.SPS
	pps := fh264.PPS

	if !passThroughFlag {
		if sps == nil || pps == nil {
			if sps == nil {
				log.Warn().Msg("SPS not present")
			}
			if pps == nil {
				log.Warn().Msg("PPS not present")
			}
		}
	}

	h264Encoder := rtph264.Encoder{
		PayloadType:    96,
		PayloadMaxSize: 1200,
	}
	h264Encoder.Init()

	// setup RTP->H264 decoder
	rtpDec := &rtph264.Decoder{}
	rtpDec.Init()

	// setup a single media
	_, err := c.Setup(session.BaseURL, mediaH264, 0, 0)
	if err != nil {
		log.Error().Err(err).Msg("Failed to start rtsp")
		rtspTerminated <- true
		return
	}

	var lastRTPts uint32

	firstKeyFrame := false

	// buffers nalus for the same timestamp (rtpTimestamp)
	nalusBuffer := [][]byte{}

	onRTPPacket := func(pkt *rtp.Packet) {
		log.Trace().Msgf("Packet received ts %d-%d", pkt.SequenceNumber, pkt.Timestamp)
		if lastRTPts != 0 {
			if pkt.Timestamp < lastRTPts {
				log.Info().Msg("Warning: Non monotonic ts. Probably a B-Frame, but B-frames not supported.")
			}
		}

		if (firstKeyFrame || passThroughFlag) && lastRTPts != pkt.Timestamp && len(nalusBuffer) > 0 {

			if (containsH264SEI(nalusBuffer) || containsH264KeyFrame(nalusBuffer)) && !containsH264PPS(nalusBuffer) {
				log.Debug().Msgf("Prepending sps and pps to keyframe or refresh-sync")
				spsAndPPS := [][]byte{sps, pps}
				nalusBuffer = append(spsAndPPS, nalusBuffer...)
			}

			// last frame is complete, so forward nalus and clear the buffer
			if err := forwardh264(nalusBuffer, &h264Encoder, videoTrack, lastRTPts); err != nil {
				log.Error().Err(err).Msg("Failed to forward")
				return
			}
			nalusBuffer = [][]byte{}
		}

		// decode H264 NALUs from the RTP packet
		nalus, err := rtpDec.Decode(pkt)
		if err != nil {
			if err != rtph264.ErrMorePacketsNeeded {
				log.Warn().Msgf("Decode failed: %s", err)
			}
			return
		}

		debugH264NALUs(nalus)

		// append to our buffer
		// This buffer contains all nalus for a timestamp
		nalusBuffer = append(nalusBuffer, nalus...)

		if !passThroughFlag {
			if !firstKeyFrame {
				firstKeyFrame = containsH264KeyFrame(nalusBuffer) || containsH264SEI(nalusBuffer)
			}
		}

		lastRTPts = pkt.Timestamp
	}

	c.OnPacketRTP(mediaH264, fh264, onRTPPacket)

	_, err = c.Play(nil)
	if err != nil {
		log.Error().Err(err).Msg("Failed to start rtsp")
		rtspTerminated <- true
		return
	}

	// wait until a fatal error
	if err := c.Wait(); err != nil {
		log.Error().Err(err).Msg("RTSP finished with err")
	}

	rtspTerminated <- true

}
