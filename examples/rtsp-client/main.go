package main

import (
	"fmt"
	"log"
	"net/url"
	"os"
	"strings"

	"github.com/aler9/gortsplib"
	"github.com/aler9/gortsplib/pkg/base"
	rtsph264 "github.com/aler9/gortsplib/pkg/h264"
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
	// start rtsp-listener
	//
	go func() {

		// Make ReadBufferSize large enough to fit whole keyframes.
		// Some IP-Cams rely on ip-fragmentation which results
		// in large UDP packets.
		c := gortsplib.Client{ReadBufferSize: 2 << 20}
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

		var lastRTPts uint32

		firstKeyFrame := false

		c.OnPacketRTP = func(trackID int, pkt *rtpv2.Packet) {

			if trackID != h264TrackID {
				return
			}

			if lastRTPts != 0 {
				if pkt.Timestamp < lastRTPts {
					log.Printf("Warning: Non monotonic ts. Probably a B-Frame, but B-frames not supported.")
				}
			}
			lastRTPts = pkt.Timestamp

			// decode H264 NALUs from the RTP packet
			nalus, ptsDur, err := rtpDec.Decode(pkt)
			if err != nil {
				//log.Printf("Decode failed: %s", err)
				return
			}

			if !passThroughFlag {
				if len(nalus) != 1 {
					log.Printf("Warning: Tested only with Decode returning 1 nalu at a time.")
				}

				typ := rtsph264.NALUType(nalus[0][0] & 0x1F)
				//log.Println("typ:", typ.String())
				switch typ {
				case rtsph264.NALUTypeSPS:
					// ignore new SPS. Take it from the track-info.
					return
				case rtsph264.NALUTypePPS:
					// ignore new PPS. Take it from the track-info.
					return
				case rtsph264.NALUTypeIDR:
					// prepend keyframe with SPS and PPS
					nalus = append([][]byte{pps}, nalus...)
					nalus = append([][]byte{sps}, nalus...)
					firstKeyFrame = true
				}

				// wait for first key-frame
				if !firstKeyFrame {
					return
				}
			}

			// convert nalus to rtp-packets
			pkts, err := h264Encoder.Encode(nalus, ptsDur)
			if err != nil {
				log.Fatalf("error while encoding H264: %v", err)
			}

			//log.Printf("Created %d packets", len(pkts))

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

				//log.Printf("Final ts: %d seq: %d len: %d mark: %v", pktv1.Timestamp, pktv1.SequenceNumber,
				//	len(pktv1.Payload), pktv1.Marker)

				err = videoTrack.WriteRTP(&pktv1)
				if err != nil {
					log.Printf("Failed to write h264 sample: %s", err)
					return
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
