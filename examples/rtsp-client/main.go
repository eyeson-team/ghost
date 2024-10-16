package main

import (
	"fmt"
	"log"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"

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

func main() {
	rootCommand.Flags().StringVarP(&apiEndpointFlag, "api-endpoint", "", "https://api.eyeson.team", "Set api-endpoint")
	rootCommand.Flags().StringVarP(&userFlag, "user", "", "rtsp-test", "User name to use")
	rootCommand.Flags().StringVarP(&userIDFlag, "user-id", "", "", "User id to use")
	rootCommand.Flags().StringVarP(&roomIDFlag, "room-id", "", "", "Room ID. If left empty, a new meeting will be created on each request")
	rootCommand.Flags().BoolVarP(&verboseFlag, "verbose", "v", false, "verbose output")
	rootCommand.Flags().BoolVarP(&widescreenFlag, "widescreen", "", true, "start room in widescreen mode")
	rootCommand.Flags().BoolVarP(&passThroughFlag, "passthrough", "", false, "if true just passthrough all H264 NAL-Units")

	rootCommand.Execute()
}

// Get a room depending on the provided api-key or guestlink.
func getRoom(apiKeyOrGuestlink, apiEndpoint, user, roomID, userID string) (*eyeson.UserService, error) {
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
		client, err := eyeson.NewClient("")
		if err != nil {
			return nil, err
		}
		baseURL, _ := url.Parse(apiEndpoint)
		client.BaseURL = baseURL
		return client.Rooms.GuestJoin(guestToken[0], userID, user, "")

	}
	// let's assume we have an apiKey, so fire up a new meeting
	client, err := eyeson.NewClient(apiKeyOrGuestlink)
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

	room, err := getRoom(apiKeyOrGuestlink, apiEndpoint, user, roomID, userID)
	if err != nil {
		log.Println("Failed to get room")
		return
	}
	log.Println("Waiting for room to become ready")
	err = room.WaitReady()
	if err != nil {
		log.Fatalf("Failed: %s", err)
	}

	log.Println("Guest-link:", room.Data.Links.GuestJoin)
	log.Println("GUI-link:", room.Data.Links.Gui)

	eyesonClient, err := ghost.NewClient(room.Data,
		ghost.WithForceH264Codec(),
		ghost.WithSendOnly())
	if err != nil {
		log.Fatalf("Failed to create eyeson-client %s", err)
	}
	defer eyesonClient.Destroy()

	eyesonClient.SetTerminatedHandler(func() {
		log.Println("Call terminated")
		os.Exit(0)
	})

	if verboseFlag {
		eyesonClient.SetDataChannelHandler(func(data []byte) {
			log.Printf("DC message: %s\n", string(data))
		})
	}

	rtspTerminatedCh := make(chan bool)
	eyesonClient.SetConnectedHandler(func(connected bool, localVideoTrack ghost.RTPWriter,
		localAudioTrack ghost.RTPWriter) {
		log.Println("Webrtc connected. Connecting to ", rtspConnectURL)
		setupRtspClient(localVideoTrack, rtspConnectURL, rtspTerminatedCh)

	})

	if err := eyesonClient.Call(); err != nil {
		log.Println("Failed to call:", err)
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

	log.Println("RTSP connection is done. So terminating this call")
	// terminate this call
	eyesonClient.TerminateCall()
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
			log.Printf("Parse RTSP-Url failed with %s.", err)
			rtspTerminated <- true
			return
		}

		// connect to the server
		err = c.Start(u.Scheme, u.Host)
		if err != nil {
			log.Printf("Connecting to rtsp server failed with %s.", err)
			rtspTerminated <- true
			return
		}
		defer c.Close()

		// find published tracks
		session, baseURL, err := c.Describe(u)
		if err != nil {
			log.Printf("Connecting to rtsp server failed with %s.", err)
			rtspTerminated <- true
			return
		}

		log.Println("baseurl:", baseURL)

		var fh264 *format.H264
		mediaH264 := session.FindFormat(&fh264)

		if mediaH264 == nil {
			log.Printf("No h264 media found")
			rtspTerminated <- true
			return
		}

		sps := fh264.SPS
		pps := fh264.PPS

		if !passThroughFlag {
			if sps == nil || pps == nil {
				log.Printf("SPS or PPS not present")
				rtspTerminated <- true
				return
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
			log.Printf("Failed to start rtsp: %s", err)
			rtspTerminated <- true
			return
		}

		var lastRTPts uint32

		firstKeyFrame := false

		onRTPPacket := func(pkt *rtp.Packet) {

			if lastRTPts != 0 {
				if pkt.Timestamp < lastRTPts {
					log.Printf("Warning: Non monotonic ts. Probably a B-Frame, but B-frames not supported.")
				}
			}
			lastRTPts = pkt.Timestamp

			// decode H264 NALUs from the RTP packet
			nalus, err := rtpDec.Decode(pkt)
			if err != nil {
				//log.Printf("Decode failed: %s", err)
				return
			}

			if !passThroughFlag {
				if len(nalus) != 1 {
					log.Printf("Warning: Tested only with Decode returning 1 nalu at a time.")
				}

				for _, nalu := range nalus {
					typ := rtsph264.NALUType(nalu[0] & 0x1F)
					//log.Printf("type %s:\n", typ.String())
					switch typ {
					case rtsph264.NALUTypeIDR:
						// prepend keyframe with SPS and PP
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

			// convert nalus to rtp-packets
			pkts, err := h264Encoder.Encode(nalus)
			if err != nil {
				log.Fatalf("error while encoding H264: %v", err)
			}

			for _, pkt := range pkts {
				//log.Printf("Final ts: %d seq: %d len: %d mark: %v", pktv1.Timestamp, pktv1.SequenceNumber,
				//	len(pktv1.Payload), pktv1.Marker)
				pkt.Timestamp = lastRTPts
				err = videoTrack.WriteRTP(pkt)
				if err != nil {
					log.Printf("Failed to write h264 sample: %s", err)
					return
				}
			}

		}

		c.OnPacketRTP(mediaH264, fh264, onRTPPacket)

		_, err = c.Play(nil)
		if err != nil {
			log.Printf("Failed to start rtsp: %s", err)
			rtspTerminated <- true
			return
		}

		// wait until a fatal error
		if err := c.Wait(); err != nil {
			log.Printf("RTSP finished with err: %s", err)
		}

		rtspTerminated <- true

	}()
}
