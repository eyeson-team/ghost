package main

import (
	"fmt"
	"log"
	"net/url"
	"os"
	"strings"

	"github.com/aler9/gortsplib/pkg/h264"
	"github.com/aler9/gortsplib/pkg/rtph264"
	"github.com/eyeson-team/eyeson-go"
	ghost "github.com/eyeson-team/ghost/v2"
	"github.com/notedit/rtmp/av"
	rtmph264 "github.com/notedit/rtmp/codec/h264"
	"github.com/notedit/rtmp/format"
	"github.com/notedit/rtmp/format/rtmp"
	"github.com/spf13/cobra"
)

var (
	apiEndpointFlag    string
	userFlag           string
	roomIDFlag         string
	rtmpListenAddrFlag string
	verboseFlag        bool

	rootCommand = &cobra.Command{
		Use:   "rtmp-server [flags] $API_KEY|$GUEST_LINK",
		Short: "rtmp-server",
		Args:  cobra.MinimumNArgs(1),
		PreRun: func(cmd *cobra.Command, args []string) {

		},
		Run: func(cmd *cobra.Command, args []string) {
			rtmpServerExample(args[0], apiEndpointFlag,
				userFlag, roomIDFlag, rtmpListenAddrFlag)
		},
	}
)

func main() {
	rootCommand.Flags().StringVarP(&apiEndpointFlag, "api-endpoint", "", "https://api.eyeson.team", "Set api-endpoint")
	rootCommand.Flags().StringVarP(&userFlag, "user", "", "rtmp-test", "User name to use")
	rootCommand.Flags().StringVarP(&roomIDFlag, "room-id", "", "", "Room ID. If left empty, a new meeting will be created on each request")
	rootCommand.Flags().StringVarP(&rtmpListenAddrFlag, "rtmp-listen-addr", "", "rtmp://0.0.0.0:1935", "rtmp address this server shall listen for")
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

func rtmpServerExample(apiKeyOrGuestlink, apiEndpoint, user, roomID, rtmpListenAddr string) {

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

	rtmpTerminatedCh := make(chan bool)
	eyesonClient.SetConnectedHandler(func(connected bool, localVideoTrack ghost.RTPWriter,
		localAudioTrack ghost.RTPWriter) {
		log.Println("Webrtc connected. Starting rtmp-server")
		setupRtmpServer(localVideoTrack, rtmpListenAddr, rtmpTerminatedCh)

	})

	if err := eyesonClient.Call(); err != nil {
		log.Println("Failed to call:", err)
		return
	}

	// Block until rtmp-connection is done
	select {
	case <-rtmpTerminatedCh:
		break
	}

	log.Println("RTMP connection is done. So terminating this call")
	// terminate this call
	eyesonClient.TerminateCall()
}

func setupRtmpServer(videoTrack ghost.RTPWriter, listenAddr string, rtmpTerminated chan<- bool) {
	//
	// start rtmp-listener
	//
	go func() {

		uo := format.URLOpener{OnNewRtmpConn: func(c *rtmp.Conn) {
			log.Println("New client connected")
		}}

		log.Println("RTMP server listening: ", listenAddr)

		reader, err := uo.Open("@" + listenAddr)
		if err != nil {
			log.Println("Err:", err)
			return
		}

		var h264Encoder *rtph264.Encoder
		h264Encoder = rtph264.NewEncoder(96, nil, nil, nil)

		sps := []byte{}
		pps := []byte{}

		for {
			packet, err := reader.ReadPacket()
			if err != nil {
				log.Println("Failed to read packet:", err)
				rtmpTerminated <- true
				return
			}

			switch packet.Type {
			case av.H264DecoderConfig:
				// read SPS and PPS and save them so those can be
				// prepended to each keyframe.
				// A different solution would be to signal the sprops via sdp.
				// But this would require to start the call _after_ the rtmp-client
				// is connected.
				codec, err := rtmph264.FromDecoderConfig(packet.Data)
				if err != nil {
					log.Fatalf("Failed to decode decoder-config:", err)
				}

				if len(codec.SPS) > 0 {
					sps = codec.SPS[0]
				}
				if len(codec.PPS) > 0 {
					pps = codec.PPS[0]
				}

			case av.H264:

				// rtmp h264 packet uses AVCC bit-stream
				// extract nalus from that bitstream
				nalus, err := h264.DecodeAVCC(packet.Data)
				if err != nil {
					log.Fatalf("Failed to decode packet:", err)
				}

				// only prepend keyframes with sps and pps
				if packet.IsKeyFrame {
					nalus = append(nalus, sps)
					nalus = append(nalus, pps)
				}

				// convert nalus to rtp-packets
				pkts, err := h264Encoder.Encode(nalus, packet.Time)
				if err != nil {
					log.Fatalf("error while encoding H264: %v", err)
				}

				for _, pkt := range pkts {
					err = videoTrack.WriteRTP(pkt)
					if err != nil {
						log.Printf("Failed to write h264 sample: %s", err)
						return
					}
				}
			}
		}
	}()
}
