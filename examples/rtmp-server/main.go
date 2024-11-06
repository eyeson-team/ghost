package main

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/bluenviron/gortsplib/v4/pkg/format/rtph264"
	"github.com/bluenviron/mediacommon/pkg/codecs/h264"
	"github.com/eyeson-team/eyeson-go"
	"github.com/eyeson-team/ghost/v2"
	"github.com/notedit/rtmp/av"
	rtmph264 "github.com/notedit/rtmp/codec/h264"
	"github.com/notedit/rtmp/format/rtmp"
	"github.com/rs/zerolog"
	log "github.com/rs/zerolog/log"
	"github.com/spf13/cobra"
)

var (
	apiEndpointFlag      string
	userFlag             string
	userIDFlag           string
	roomIDFlag           string
	rtmpListenAddrFlag   string
	verboseFlag          bool
	traceFlag            bool
	jitterQueueLenMSFlag int32
	customCAFileFlag     string
	widescreenFlag       bool
	quietFlag            bool

	rootCommand = &cobra.Command{
		Use:   "rtmp-server [flags] $API_KEY|$GUEST_LINK",
		Short: "rtmp-server",
		Args:  cobra.MinimumNArgs(1),
		PreRun: func(cmd *cobra.Command, args []string) {

		},
		Run: func(cmd *cobra.Command, args []string) {
			rtmpServerExample(args[0], apiEndpointFlag,
				userFlag, roomIDFlag, rtmpListenAddrFlag,
				userIDFlag)
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
		zerolog.ConsoleWriter{
			Out: os.Stderr, TimeFormat: "15:04:05.000"})
	zerolog.TimeFieldFormat = zerolog.TimeFormatUnixMs
	zerolog.SetGlobalLevel(zerolog.InfoLevel)
	cobra.OnInitialize(initLogging)

	rootCommand.Flags().StringVarP(&apiEndpointFlag, "api-endpoint", "", "https://api.eyeson.team", "Set api-endpoint")
	rootCommand.Flags().StringVarP(&userFlag, "user", "", "rtmp-test", "User name to use")
	rootCommand.Flags().StringVarP(&userIDFlag, "user-id", "", "", "User id to use")
	rootCommand.Flags().StringVarP(&roomIDFlag, "room-id", "", "", "Room ID. If left empty, a new meeting will be created on each request")
	rootCommand.Flags().StringVarP(&rtmpListenAddrFlag, "rtmp-listen-addr", "", "rtmp://0.0.0.0:1935", "rtmp address this server shall listen to")
	rootCommand.Flags().BoolVarP(&verboseFlag, "verbose", "v", false, "verbose output")
	rootCommand.Flags().BoolVarP(&traceFlag, "trace", "", false, "trace output")
	rootCommand.Flags().BoolVarP(&quietFlag, "quiet", "q", false, "no logging output")
	rootCommand.Flags().Int32VarP(&jitterQueueLenMSFlag, "delay", "", 150, "delay in ms")
	rootCommand.Flags().StringVarP(&customCAFileFlag, "custom-ca", "", "", "custom CA file")
	rootCommand.Flags().BoolVarP(&widescreenFlag, "widescreen", "", true, "start room in widescreen mode")

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

func rtmpServerExample(apiKeyOrGuestlink, apiEndpoint, user, roomID,
	rtmpListenAddr, userID string) {

	room, err := getRoom(apiKeyOrGuestlink, apiEndpoint, user, roomID, userID,
		customCAFileFlag)
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
		ghost.WithForceH264Codec(),
		ghost.WithSendOnly(),
	}
	if len(customCAFileFlag) > 0 {
		clientOptions = append(clientOptions, ghost.WithCustomCAFile(customCAFileFlag))
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

	rtmpTerminatedCh := make(chan bool)
	eyesonClient.SetConnectedHandler(func(connected bool, localVideoTrack ghost.RTPWriter,
		localAudioTrack ghost.RTPWriter) {
		log.Debug().Msg("Webrtc connected. Starting rtmp-server")
		setupRtmpServer(localVideoTrack, rtmpListenAddr, rtmpTerminatedCh)

	})

	if err := eyesonClient.Call(); err != nil {
		log.Error().Err(err).Msg("Failed to call")
		return
	}

	// install signal-handler
	chStop := make(chan os.Signal, 1)
	signal.Notify(chStop, syscall.SIGINT, syscall.SIGTERM)

	// Block until rtmp-connection is done
	select {
	case <-chStop:
		break
	case <-rtmpTerminatedCh:
		break
	}

	log.Info().Msg("RTMP connection is done. So terminating this call")
	// terminate this call
	eyesonClient.TerminateCall()
}

func setupRtmpServer(videoTrack ghost.RTPWriter, listenAddr string, rtmpTerminated chan<- bool) {
	//
	// start rtmp-listener
	//
	go func() {

		rtmpServer := rtmp.NewServer()

		url, _ := url.Parse(listenAddr)
		host := rtmp.UrlGetHost(url)

		var err error
		var lis net.Listener
		if lis, err = net.Listen("tcp", host); err != nil {
			log.Error().Err(err).Msgf("Failed to start RTMP server")
			rtmpTerminated <- true
			return
		}

		log.Info().Msgf("RTMP server listening: %s", listenAddr)

		// Init the rtph264-rtp-header-encoder only once,
		// and reuse if another rtmp-client connects.
		h264Encoder := rtph264.Encoder{
			PayloadType:    96,
			PayloadMaxSize: 1200,
		}
		h264Encoder.Init()

		rtmpServer.HandleConn = func(c *rtmp.Conn, nc net.Conn) {
			log.Debug().Msg("New rtmp-conn created")

			sps := []byte{}
			pps := []byte{}

			videoJB, err := NewVideoJitterBuffer(videoTrack,
				time.Duration(jitterQueueLenMSFlag)*time.Millisecond)
			if err != nil {
				log.Fatal().Err(err).Msg("Failed to setup vjb")
			}
			defer videoJB.Close()

			var currentTS uint32 = 0
			naluBuffer := [][]byte{}

			for {
				packet, err := c.ReadPacket()
				if err != nil {
					log.Info().Err(err).Msg("Failed to read packet")
					return
				}

				//log.Printf("DBG: Packet ts from rtmp: %s", packet.Time)

				switch packet.Type {
				case av.H264DecoderConfig:
					// read SPS and PPS and save them so those can be
					// prepended to each keyframe.
					// A different solution would be to signal the sprops via sdp.
					// But this would require to start the call _after_ the rtmp-client
					// is connected.
					codec, err := rtmph264.FromDecoderConfig(packet.Data)
					if err != nil {
						log.Fatal().Err(err).Msg("Failed to decode decoder-config")
					}

					if len(codec.SPS) > 0 {
						sps = codec.SPS[0]
					}
					if len(codec.PPS) > 0 {
						pps = codec.PPS[0]
					}

				case av.H264:
					newTS := uint32(packet.Time.Seconds() * 90000)
					if newTS != currentTS {
						// write all buffers to webrtc
						// convert nalus to rtp-packets
						pkts, err := h264Encoder.Encode(naluBuffer)
						if err != nil {
							log.Error().Err(err).Msg("error while encoding H264")
							continue
						}

						for _, pkt := range pkts {
							pkt.Header.Timestamp = currentTS
							log.Trace().Msgf("Final ts: %d seq: %d len: %d mark: %v", pkt.Timestamp,
								pkt.SequenceNumber, len(pkt.Payload), pkt.Marker)
							err = videoJB.WriteRTP(pkt)
							if err != nil {
								log.Error().Err(err).Msg("Failed to write h264 sample")
								return
							}
						}
						// clear
						naluBuffer = [][]byte{}
						currentTS = newTS
					}

					// rtmp h264 packet uses AVCC bit-stream
					// extract nalus from that bitstream
					nalus, err := h264.AVCCUnmarshal(packet.Data)
					if err != nil {
						log.Error().Err(err).Msg("Failed to decode packet")
						continue
					}

					debugNALUTypes := false
					if debugNALUTypes {
						for _, n := range nalus {
							naluType := h264.NALUType(n[0] & 0x1F)
							log.Debug().Msgf("nalu-type: %v-%s", naluType, naluType.String())
						}
					}

					// Check, if there is only one NALU with an SEI.
					// If so, skip it. Those SEI-packets lead to
					// depackaging/decoding issues and seem not to be relevant.
					if len(nalus) == 1 {
						naluType := h264.NALUType(nalus[0][0] & 0x1F)
						if naluType == h264.NALUTypeSEI {
							//log.Printf("skipping nalu-type SEI")
							continue
						}
					}

					// only prepend keyframes with sps and pps
					if packet.IsKeyFrame {
						nalus = append(nalus, sps)
						nalus = append(nalus, pps)
					}
					naluBuffer = append(naluBuffer, nalus...)
				}
			}
		}

		for {
			nc, err := lis.Accept()
			if err != nil {
				time.Sleep(time.Second)
				continue
			}
			log.Debug().Msg("New Client connected")
			rtmpServer.HandleNetConn(nc)
		}

	}()
}
