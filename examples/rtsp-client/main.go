package main

import (
	"fmt"
	"log"
	"net/url"
	"os"
	"strings"

	"github.com/aler9/gortsplib"
	"github.com/aler9/gortsplib/pkg/base"
	"github.com/aler9/gortsplib/pkg/rtph264"
	"github.com/eyeson-team/eyeson-go"
	ghost "github.com/eyeson-team/ghost/v2"
	"github.com/pion/rtp"
	rtpv2 "github.com/pion/rtp/v2"
	"github.com/spf13/cobra"
)

var (
	apiEndpointFlag string
	userFlag        string
	roomIDFlag      string
	verboseFlag     bool

	rootCommand = &cobra.Command{
		Use:   "rtsp-client [flags] $API_KEY|$GUEST_LINK RTSP_CONNECT_URL",
		Short: "rtsp-client",
		Args:  cobra.MinimumNArgs(2),
		PreRun: func(cmd *cobra.Command, args []string) {

		},
		Run: func(cmd *cobra.Command, args []string) {
			rtspClientExample(args[0], args[1], apiEndpointFlag,
				userFlag, roomIDFlag)
		},
	}
)

func main() {
	rootCommand.Flags().StringVarP(&apiEndpointFlag, "api-endpoint", "", "https://api.eyeson.team", "Set api-endpoint")
	rootCommand.Flags().StringVarP(&userFlag, "user", "", "rtsp-test", "User name to use")
	rootCommand.Flags().StringVarP(&roomIDFlag, "room-id", "", "", "Room ID. If left empty, a new meeting will be created on each request")
	rootCommand.Flags().BoolVarP(&verboseFlag, "verbose", "v", false, "verbose output")

	rootCommand.Execute()
}

// Get a room depending on the provided api-key or guestlink.
func getRoom(apiKeyOrGuestlink, apiEndpoint, user, roomID string) (*eyeson.UserService, error) {
	// determine if we have a guestlink
	if strings.HasPrefix(apiKeyOrGuestlink, "http") {
		// join as guest

		// guest-link: https://app.eyeson.team/?guest=h7IHRfwnV6Yuk3QtL2jbktuh
		guestPos := strings.LastIndex(apiKeyOrGuestlink, "guest=")
		if guestPos == -1 {
			return nil, fmt.Errorf("Invalid guest-link")
		}
		guestToken := apiKeyOrGuestlink[guestPos+len("guest="):]

		client := eyeson.NewClient("")
		baseURL, _ := url.Parse(apiEndpoint)
		client.BaseURL = baseURL
		return client.Rooms.GuestJoin(guestToken, roomID, user, "")

	} else {
		// let's assume we have an apiKey, so fire up a new meeting
		client := eyeson.NewClient(apiKeyOrGuestlink)
		baseURL, _ := url.Parse(apiEndpoint)
		client.BaseURL = baseURL
		return client.Rooms.Join(roomID, user, nil)
	}
}

func rtspClientExample(apiKeyOrGuestlink, rtspConnectURL, apiEndpoint, user, roomID string) {

	room, err := getRoom(apiKeyOrGuestlink, apiEndpoint, user, roomID)
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

	// Block until rtsp-connection is done
	select {
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
	// start rtmp-listener
	//
	go func() {

		c := gortsplib.Client{}
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
		tracks, baseURL, _, err := c.Describe(u)
		if err != nil {
			log.Printf("Connecting to rtsp server failed with %s.", err)
			rtspTerminated <- true
			return
		}

		log.Println("tracks:", tracks)
		log.Println("baseurl:", baseURL)

		// find the H264 track
		h264TrackID, h264track := func() (int, *gortsplib.TrackH264) {
			for i, track := range tracks {
				if h264track, ok := track.(*gortsplib.TrackH264); ok {
					return i, h264track
				}
			}
			return -1, nil
		}()
		if h264TrackID < 0 {
			log.Printf("No h264 track found")
			rtspTerminated <- true
			return
		}

		sps := h264track.SPS()
		pps := h264track.PPS()

		if sps == nil || pps == nil {
			log.Printf("SPS or PPS not present")
			rtspTerminated <- true
			return
		}

		h264Encoder := rtph264.Encoder{
			PayloadType:    96,
			PayloadMaxSize: 1200,
		}
		h264Encoder.Init()

		// setup RTP->H264 decoder
		rtpDec := &rtph264.Decoder{}
		rtpDec.Init()

		c.OnPacketRTP = func(trackID int, pkt *rtpv2.Packet) {
			//log.Printf("trackID %d packet received: %s", pkt.String)

			if trackID == h264TrackID {
				// decode H264 NALUs from the RTP packet
				nalus, ptsDur, err := rtpDec.Decode(pkt)
				if err != nil {
					return
				}

				// prepend sps and pps. Would be required for keyframes only.
				// So determine keyframes.
				nalus = append(nalus, sps)
				nalus = append(nalus, pps)

				// convert nalus to rtp-packets
				pkts, err := h264Encoder.Encode(nalus, ptsDur)
				if err != nil {
					log.Fatalf("error while encoding H264: %v", err)
				}

				for _, pkt := range pkts {
					// convert rtpv2 to rtpv1
					b, err := pkt.Marshal()
					if err != nil {
						log.Printf("Failed to marshal v2-packet")
					}
					var pktv1 rtp.Packet
					err = pktv1.Unmarshal(b)
					if err != nil {
						log.Printf("Failed to unmarshal to v1-packet")
					}

					err = videoTrack.WriteRTP(&pktv1)
					if err != nil {
						log.Printf("Failed to write h264 sample: %s", err)
						return
					}
				}

			}
		}

		err = c.SetupAndPlay(tracks, baseURL)
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
