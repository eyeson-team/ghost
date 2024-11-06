package main

import (
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
	"github.com/rs/zerolog"
	log "github.com/rs/zerolog/log"
	"github.com/spf13/cobra"
)

var (
	apiEndpointFlag  string
	userFlag         string
	userIDFlag       string
	roomIDFlag       string
	verboseFlag      bool
	traceFlag        bool
	customCAFileFlag string
	widescreenFlag   bool
	quietFlag        bool
	loopFlag         bool

	rootCommand = &cobra.Command{
		Use:   "ghost-player [flags] $API_KEY|$GUEST_LINK VIDEO_FILE",
		Short: "ghost-player",
		Args:  cobra.MinimumNArgs(2),
		PreRun: func(cmd *cobra.Command, args []string) {

		},
		Run: func(cmd *cobra.Command, args []string) {
			videoPlayerExample(args[0], args[1], apiEndpointFlag,
				userFlag, roomIDFlag,
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
	rootCommand.Flags().StringVarP(&userFlag, "user", "", "ghost-player", "User name to use")
	rootCommand.Flags().StringVarP(&userIDFlag, "user-id", "", "", "User id to use")
	rootCommand.Flags().StringVarP(&roomIDFlag, "room-id", "", "", "Room ID. If left empty, a new meeting will be created on each request")
	rootCommand.Flags().BoolVarP(&verboseFlag, "verbose", "v", false, "verbose output")
	rootCommand.Flags().BoolVarP(&traceFlag, "trace", "", false, "trace output")
	rootCommand.Flags().BoolVarP(&quietFlag, "quiet", "q", false, "no logging output")
	rootCommand.Flags().StringVarP(&customCAFileFlag, "custom-ca", "", "", "custom CA file")
	rootCommand.Flags().BoolVarP(&widescreenFlag, "widescreen", "", true, "start room in widescreen mode")
	rootCommand.Flags().BoolVarP(&loopFlag, "loop", "", true, "Restart video-playback on EOF")

	rootCommand.Execute()
}

type rtpSender struct {
	track      ghost.RTPWriter
	sequencer  rtp.Sequencer
	packetizer rtp.Packetizer
}

func newRtpSender(track ghost.RTPWriter) (*rtpSender, error) {
	var rtpOutboundMTU uint16 = 1200
	sequencer := rtp.NewRandomSequencer()
	payloader := &codecs.VP8Payloader{}
	var codecClockRate uint32 = 90000
	packetizer := rtp.NewPacketizer(
		rtpOutboundMTU,
		0, // Value is handled when writing
		0, // Value is handled when writing
		payloader,
		sequencer,
		codecClockRate,
	)

	return &rtpSender{
		track:      track,
		sequencer:  sequencer,
		packetizer: packetizer,
	}, nil
}

func (rs *rtpSender) send(sampleData []byte) {
	var samples uint32 = 90000
	packets := rs.packetizer.Packetize(sampleData, samples)
	writeErrs := []error{}
	for _, p := range packets {
		if err := rs.track.WriteRTP(p); err != nil {
			writeErrs = append(writeErrs, err)
		}
	}
	if len(writeErrs) > 0 {
		log.Warn().Msgf("%d rtp write-errors occured", len(writeErrs))
	}
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

func ingestControl(videoFile string, localVideoTrack ghost.RTPWriter, loop bool) {
	for {
		err := ingestVideo(videoFile, localVideoTrack)
		if err != nil {
			log.Warn().Err(err).Msg("Ingest video failed")
			return
		}
		if !loop {
			break
		}
		log.Info().Msg("Restarting video playback")
	}
}

func ingestVideo(videoFile string, localVideoTrack ghost.RTPWriter) error {
	// Open the WebM file
	file, err := os.Open(videoFile)
	if err != nil {
		log.Warn().Err(err).Msgf("Error opening file %s", videoFile)
		return err
	}
	defer file.Close()

	rs, _ := newRtpSender(localVideoTrack)
	ctx := &webm.WebM{}

	webmReader, err := webm.Parse(file, ctx)
	if err != nil {
		log.Warn().Err(err).Msg("Parsing webm failed")
		return err
	}

	videoTrack := ctx.FindFirstVideoTrack()
	if videoTrack == nil {
		log.Warn().Err(err).Msg("No videotrack")
		return err
	}
	// todo: handle codec selection
	//fmt.Printf("Video-Codec: %v - %v\n", videoTrack.CodecName, videoTrack.CodecID)
	log.Debug().Msgf("Webm duration is %v", ctx.Segment.GetDuration())
	log.Debug().Msgf("Webm video codec is %v", videoTrack.CodecID)
	started := time.Now()

	for {
		select {
		case packet, ok := <-webmReader.Chan:
			if !ok {
				log.Warn().Err(err).Msg("No videotrack")
				return err
			}
			if len(packet.Data) == 0 {
				// we're done
				log.Debug().Msg("File EOF")
				return nil
			}
			// select video
			if packet.TrackNumber != videoTrack.TrackNumber {
				continue
			}

			// now wait till it's ok to send that packet
			for {
				elapsed := time.Since(started)
				if packet.Timecode < elapsed {
					rs.send(packet.Data)
					break
				}
				time.Sleep(20 * time.Millisecond)
			}
		}
	}
}

func videoPlayerExample(apiKeyOrGuestlink, videoFile, apiEndpoint, user, roomID,
	userID string) {

	file, err := os.Open(videoFile)
	if err != nil {
		log.Error().Err(err).Msg("Failed to open videoFile")
		return
	}
	file.Close()

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
		go ingestControl(videoFile, localVideoTrack, loopFlag)
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

	log.Info().Msg("Stopping. So terminating this call")
	// terminate this call
	eyesonClient.TerminateCall()
}
