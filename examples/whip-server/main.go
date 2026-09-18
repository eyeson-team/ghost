// whip-server is a ghost example that turns an eyeson meeting into a WHIP
// ingest endpoint.
//
// A WHIP sender (OBS Studio, GStreamer whipsink, ffmpeg, a browser, ...)
// publishes its WebRTC stream to this server. The received RTP packets are
// forwarded, without transcoding, into an eyeson meeting through the ghost
// client.
//
//	OBS ──WHIP (HTTP + WebRTC)──▶ whip-server ──ghost/SEPP (WebRTC)──▶ eyeson meeting
//
// The video codec is not fixed: it is negotiated with the sender and the eyeson
// connection is then built to match, so a VP9 or AV1 capable sender keeps its
// codec all the way into the meeting.
package main

import (
	"fmt"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/eyeson-team/eyeson-go"
	ghost "github.com/eyeson-team/ghost/v2"
	"github.com/rs/zerolog"
	log "github.com/rs/zerolog/log"
	"github.com/spf13/cobra"
)

var (
	// Version is set at build time, see build.sh.
	Version = "dev"
)

var (
	apiEndpointFlag        string
	userFlag               string
	userIDFlag             string
	roomIDFlag             string
	whipListenAddrFlag     string
	whipPathFlag           string
	bearerTokenFlag        string
	tlsCertFlag            string
	tlsKeyFlag             string
	videoCodecsFlag        string
	iceServersFlag         string
	advertiseICEFlag       bool
	publicIPFlag           string
	udpPortRangeFlag       string
	pliIntervalFlag        int32
	noAudioFlag            bool
	exitOnDisconnectFlag   bool
	widescreenFlag         bool
	verboseFlag            bool
	traceFlag              bool
	quietFlag              bool
	customCAFileFlag       string
	insecureSkipVerifyFlag bool

	rootCommand = &cobra.Command{
		Use:   "whip-server [flags] $API_KEY|$GUEST_LINK",
		Short: "WHIP ingest endpoint that forwards into an eyeson meeting",
		Args:  cobra.MinimumNArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			whipServerExample(args[0], apiEndpointFlag, userFlag, roomIDFlag, userIDFlag)
		},
	}
)

// Logger maps the ghost logger interface onto zerolog.
type Logger struct{}

// Error logs an error message.
func (sl *Logger) Error(format string, v ...interface{}) { log.Error().Msgf(format, v...) }

// Warn logs a warning message.
func (sl *Logger) Warn(format string, v ...interface{}) { log.Warn().Msgf(format, v...) }

// Info logs an info message.
func (sl *Logger) Info(format string, v ...interface{}) { log.Info().Msgf(format, v...) }

// Debug logs a debug message.
func (sl *Logger) Debug(format string, v ...interface{}) { log.Debug().Msgf(format, v...) }

// Trace logs a trace message.
func (sl *Logger) Trace(format string, v ...interface{}) { log.Trace().Msgf(format, v...) }

// initLogging maps the two flags onto zerolog levels. The split is what goes
// into them: --verbose is the per session detail you want while something is
// misbehaving, --trace adds the raw protocol dumps (the WHIP SDP, the data
// channel traffic), which are too bulky to carry along by default.
func initLogging() {
	switch {
	case traceFlag:
		zerolog.SetGlobalLevel(zerolog.TraceLevel)
	case verboseFlag:
		zerolog.SetGlobalLevel(zerolog.DebugLevel)
	case quietFlag:
		zerolog.SetGlobalLevel(zerolog.Disabled)
	default:
		zerolog.SetGlobalLevel(zerolog.InfoLevel)
	}
}

func main() {
	log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stderr, TimeFormat: "15:04:05.000"})
	zerolog.TimeFieldFormat = zerolog.TimeFormatUnixMs
	zerolog.SetGlobalLevel(zerolog.InfoLevel)
	cobra.OnInitialize(initLogging)

	rootCommand.Version = Version
	rootCommand.SetVersionTemplate(`{{.Version}}`)

	rootCommand.Flags().StringVarP(&apiEndpointFlag, "api-endpoint", "", "https://api.eyeson.team", "Set api-endpoint")
	rootCommand.Flags().StringVarP(&userFlag, "user", "", "whip-test", "User name to use")
	rootCommand.Flags().StringVarP(&userIDFlag, "user-id", "", "", "User id to use")
	rootCommand.Flags().StringVarP(&roomIDFlag, "room-id", "", "", "Room ID. If left empty, a new meeting will be created on each request")
	rootCommand.Flags().StringVarP(&whipListenAddrFlag, "whip-listen-addr", "", ":8100", "address the WHIP endpoint listens on")
	rootCommand.Flags().StringVarP(&whipPathFlag, "whip-path", "", "/whip", "http path of the WHIP endpoint")
	rootCommand.Flags().StringVarP(&bearerTokenFlag, "bearer-token", "", "", "if set, senders must provide this token as an Authorization: Bearer header")
	rootCommand.Flags().StringVarP(&tlsCertFlag, "tls-cert", "", "", "certificate file to serve the WHIP endpoint via https")
	rootCommand.Flags().StringVarP(&tlsKeyFlag, "tls-key", "", "", "key file to serve the WHIP endpoint via https")
	rootCommand.Flags().StringVarP(&videoCodecsFlag, "video-codecs", "", DefaultVideoCodecs, "accepted video codecs, most preferred first")
	rootCommand.Flags().StringVarP(&iceServersFlag, "ice-servers", "", "", "comma separated ice servers, e.g. turn:user:pass@host:3478. defaults to the ones the eyeson api returns, \"none\" disables them")
	rootCommand.Flags().BoolVarP(&advertiseICEFlag, "advertise-ice", "", false, "send the ice servers to senders via WHIP Link headers")
	rootCommand.Flags().StringVarP(&publicIPFlag, "public-ip", "", "", "public ip to use in host candidates, for servers behind 1:1 NAT")
	rootCommand.Flags().StringVarP(&udpPortRangeFlag, "udp-port-range", "", "", "restrict ice to a udp port range, e.g. 50000-50100")
	rootCommand.Flags().Int32VarP(&pliIntervalFlag, "pli-interval", "", 3000, "interval in ms to request a keyframe from the WHIP sender, 0 disables it")
	rootCommand.Flags().BoolVarP(&noAudioFlag, "no-audio", "", false, "do not forward the audio track")
	rootCommand.Flags().BoolVarP(&exitOnDisconnectFlag, "exit-on-disconnect", "", false, "terminate the meeting when the WHIP sender disconnects")
	rootCommand.Flags().BoolVarP(&widescreenFlag, "widescreen", "", true, "start room in widescreen mode")
	rootCommand.Flags().BoolVarP(&verboseFlag, "verbose", "v", false, "per session detail: timings, dropped packets, ice gathering")
	rootCommand.Flags().BoolVarP(&traceFlag, "trace", "", false, "everything --verbose has, plus the exchanged sdp and the data channel messages")
	rootCommand.Flags().BoolVarP(&quietFlag, "quiet", "q", false, "no logging output")
	rootCommand.Flags().StringVarP(&customCAFileFlag, "custom-ca", "", "", "custom CA file")
	rootCommand.Flags().BoolVarP(&insecureSkipVerifyFlag, "insecure", "", false, "if true don't verify remote tls certificates")

	rootCommand.Execute()
}

// getRoom returns a room depending on the provided api-key or guest link.
// This is the same helper the rtmp-server and rtsp-client examples use.
func getRoom(apiKeyOrGuestlink, apiEndpoint, user, roomID, userID, customCA string,
	insecure bool) (*eyeson.UserService, error) {
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
			return nil, fmt.Errorf("invalid guest-link")
		}
		params, err := url.ParseQuery(u.RawQuery)
		if err != nil {
			return nil, fmt.Errorf("invalid guest-link")
		}
		guestToken, ok := params["guest"]
		if !ok || len(guestToken) != 1 {
			return nil, fmt.Errorf("invalid guest-link")
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

func whipServerExample(apiKeyOrGuestlink, apiEndpoint, user, roomID, userID string) {

	codecs, err := ParseVideoCodecs(videoCodecsFlag)
	if err != nil {
		log.Error().Err(err).Msg("Invalid --video-codecs")
		return
	}

	room, err := getRoom(apiKeyOrGuestlink, apiEndpoint, user, roomID, userID,
		customCAFileFlag, insecureSkipVerifyFlag)
	if err != nil {
		log.Error().Err(err).Msg("Failed to get room")
		return
	}

	log.Debug().Msg("Waiting for room to become ready")
	if err := room.WaitReady(); err != nil {
		log.Fatal().Err(err).Msg("Failed waiting for the room")
	}

	log.Info().Msgf("Guest-link: %s", room.Data.Links.GuestJoin)
	log.Info().Msgf("GUI-link: %s", room.Data.Links.Gui)

	iceSettings, err := buildICESettings(room)
	if err != nil {
		log.Error().Err(err).Msg("Invalid ice configuration")
		return
	}

	ghostOptions := []ghost.ClientOption{
		ghost.WithCustomLogger(&Logger{}),
		ghost.WithSendOnly(),
	}
	if len(customCAFileFlag) > 0 {
		ghostOptions = append(ghostOptions, ghost.WithCustomCAFile(customCAFileFlag))
	}
	if insecureSkipVerifyFlag {
		ghostOptions = append(ghostOptions, ghost.WithInsecureSkipVerify())
	}

	doneCh := make(chan bool, 1)
	done := func() {
		select {
		case doneCh <- true:
		default:
		}
	}

	// The meeting is joined lazily: which video codec ghost has to use is only
	// known once a sender has published its offer.
	connector := NewMeetingConnector(room, ghostOptions, noAudioFlag)
	connector.OnTerminated = done
	defer connector.Close()

	whipServer := NewWHIPServer(WHIPConfig{
		ListenAddr:  whipListenAddrFlag,
		Path:        whipPathFlag,
		BearerToken: bearerTokenFlag,
		TLSCertFile: tlsCertFlag,
		TLSKeyFile:  tlsKeyFlag,
		ICE:         iceSettings,
		VideoCodecs: codecs,
		PLIInterval: time.Duration(pliIntervalFlag) * time.Millisecond,
		Connect:     connector.Connect,
		OnSessionEnded: func() {
			if !exitOnDisconnectFlag {
				log.Info().Msg("WHIP session ended, waiting for the next sender")
				return
			}
			done()
		},
	})

	if err := whipServer.Start(); err != nil {
		log.Error().Err(err).Msg("Failed to start whip-server")
		return
	}

	log.Info().Msgf("Waiting for a WHIP sender, accepted video codecs: %s", videoCodecsFlag)

	// install signal-handler
	chStop := make(chan os.Signal, 1)
	signal.Notify(chStop, syscall.SIGINT, syscall.SIGTERM)

	select {
	case <-chStop:
	case <-doneCh:
	}

	log.Info().Msg("Shutting down")
	connector.Close()
}

// buildICESettings collects the stun and turn servers for the ingest peer
// connection. By default the ones the eyeson api handed out for this room are
// reused - they are already there, they are close to the meeting, and unlike a
// public stun server the turn entry also works when both ends sit behind a
// symmetric NAT.
func buildICESettings(room *eyeson.UserService) (ICESettings, error) {
	settings := ICESettings{
		PublicIP:  publicIPFlag,
		Advertise: advertiseICEFlag,
	}

	switch {
	case strings.EqualFold(iceServersFlag, ICEServersNone):
	case iceServersFlag != "":
		servers, err := ParseICEServers(iceServersFlag)
		if err != nil {
			return settings, err
		}
		settings.Servers = servers
	default:
		settings.Servers = eyesonICEServers(room)
	}

	portMin, portMax, err := ParseUDPPortRange(udpPortRangeFlag)
	if err != nil {
		return settings, err
	}
	settings.UDPPortMin = portMin
	settings.UDPPortMax = portMax

	log.Info().Msgf("ICE: using %d server(s)", len(settings.Servers))

	return settings, nil
}

// eyesonICEServers turns what the eyeson api returned into ice servers.
func eyesonICEServers(room *eyeson.UserService) []ICEServer {
	servers := []ICEServer{}

	for _, stun := range room.Data.GetStunServers() {
		servers = append(servers, ICEServer{URL: stun})
	}

	username := room.Data.GetTurnServerUsername()
	password := room.Data.GetTurnServerPassword()
	for _, turn := range room.Data.GetTurnServerURLs() {
		servers = append(servers, ICEServer{
			URL:      turn,
			Username: username,
			Password: password,
		})
	}

	return servers
}