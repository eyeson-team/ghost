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
	"github.com/bluenviron/gortsplib/v4/pkg/format"
	"github.com/bluenviron/gortsplib/v4/pkg/format/rtph264"

	rtsph264 "github.com/bluenviron/mediacommon/pkg/codecs/h264"
	"github.com/eyeson-team/eyeson-go"
	ghost "github.com/eyeson-team/ghost/v2"
	"github.com/pion/rtp"
	"github.com/spf13/cobra"
)

var (
	apiEndpointFlag string
	userFlag        string
	userIDFlag      string
	roomIDFlag      string
	verboseFlag     bool
	widescreenFlag  bool
	passThroughFlag bool
	traceFlag       bool
	quietFlag       bool
	customCAFileFlag     string

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

	rootCommand.Execute()
}

// Get a room depending on the provided api-key or guestlink.
func getRoom(apiKeyOrGuestlink, apiEndpoint, user, roomID, userID, customCA string) (*eyeson.UserService, error) {
	clientOptions := []eyeson.ClientOption{}
	if len(customCA) > 0 {
		clientOptions = append(clientOptions, eyeson.WithCustomCAFile(customCA))
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
		client, err := eyeson.NewClient("",clientOptions...)
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

	room, err := getRoom(apiKeyOrGuestlink, apiEndpoint, user, roomID, userID, customCAFileFlag)
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

	eyesonClient, err := ghost.NewClient(room.Data,
		ghost.WithCustomLogger(&Logger{}),
		ghost.WithForceH264Codec(),
		ghost.WithSendOnly())
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
		setupRtspClient(localVideoTrack, rtspConnectURL, rtspTerminatedCh)
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

func forward(nalus [][]byte, encoder *rtph264.Encoder, videoTrack ghost.RTPWriter, rtpTimestamp uint32) error {
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
	rtspTerminated chan<- bool) {
	//
	// start rtsp-listener
	//
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

		var fh264 *format.H264
		mediaH264 := session.FindFormat(&fh264)

		if mediaH264 == nil {
			log.Error().Msg("No h264 media found")
			rtspTerminated <- true
			return
		}

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
		_, err = c.Setup(session.BaseURL, mediaH264, 0, 0)
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

			if lastRTPts != 0 {
				if pkt.Timestamp < lastRTPts {
					log.Info().Msg("Warning: Non monotonic ts. Probably a B-Frame, but B-frames not supported.")
				}
			}

			// decode H264 NALUs from the RTP packet
			nalus, err := rtpDec.Decode(pkt)
			if err != nil {
				//log.Printf("Decode failed: %s", err)
				return
			}

			if !passThroughFlag {
				for _, nalu := range nalus {
					typ := rtsph264.NALUType(nalu[0] & 0x1F)
					// log.Debug().Msgf("type %s:", typ.String())
					switch typ {
					case rtsph264.NALUTypeIDR:
						// prepend keyframe with SPS and PPS
						nalus = append([][]byte{pps}, nalus...)
						nalus = append([][]byte{sps}, nalus...)
						firstKeyFrame = true
					}
				}

				// wait for first key-frame
				if !firstKeyFrame {
					return
				}
			}

			if lastRTPts != pkt.Timestamp {
				// last frame is complete, so forward nalus and clear the buffer
				if err := forward(nalusBuffer, &h264Encoder, videoTrack, lastRTPts); err != nil {
					log.Error().Err(err).Msg("Failed to forward")
					return
				}
				nalusBuffer = [][]byte{}
			}
			nalusBuffer = append(nalusBuffer, nalus...)
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

	}()
}
