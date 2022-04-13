package main

import (
	"fmt"
	"log"
	"net"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/aler9/gortsplib/pkg/h264"
	"github.com/aler9/gortsplib/pkg/rtph264"
	"github.com/eyeson-team/eyeson-go"
	ghost "github.com/eyeson-team/ghost/v2"
	"github.com/notedit/rtmp/av"
	rtmph264 "github.com/notedit/rtmp/codec/h264"
	"github.com/notedit/rtmp/format/rtmp"
	"github.com/spf13/cobra"
)

var (
	apiEndpointFlag      string
	userFlag             string
	userIDFlag           string
	roomIDFlag           string
	rtmpListenAddrFlag   string
	verboseFlag          bool
	jitterQueueLenMSFlag int32

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

func main() {
	rootCommand.Flags().StringVarP(&apiEndpointFlag, "api-endpoint", "", "https://api.eyeson.team", "Set api-endpoint")
	rootCommand.Flags().StringVarP(&userFlag, "user", "", "rtmp-test", "User name to use")
	rootCommand.Flags().StringVarP(&userIDFlag, "user-id", "", "", "User id to use")
	rootCommand.Flags().StringVarP(&roomIDFlag, "room-id", "", "", "Room ID. If left empty, a new meeting will be created on each request")
	rootCommand.Flags().StringVarP(&rtmpListenAddrFlag, "rtmp-listen-addr", "", "rtmp://0.0.0.0:1935", "rtmp address this server shall listen to")
	rootCommand.Flags().BoolVarP(&verboseFlag, "verbose", "v", false, "verbose output")
	rootCommand.Flags().Int32VarP(&jitterQueueLenMSFlag, "delay", "", 150, "delay in ms")

	rootCommand.Execute()
}

// Get a room depending on the provided api-key or guestlink.
func getRoom(apiKeyOrGuestlink, apiEndpoint, user, roomID, userID string) (*eyeson.UserService, error) {
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
		return client.Rooms.GuestJoin(guestToken, userID, user, "")

	} else {
		// let's assume we have an apiKey, so fire up a new meeting
		client := eyeson.NewClient(apiKeyOrGuestlink)
		baseURL, _ := url.Parse(apiEndpoint)
		client.BaseURL = baseURL
		return client.Rooms.Join(roomID, user, nil)
	}
}

func rtmpServerExample(apiKeyOrGuestlink, apiEndpoint, user, roomID,
	rtmpListenAddr, userID string) {

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

	log.Println("RTMP connection is done. So terminating this call")
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
			return
		}

		log.Println("RTMP server listening: ", listenAddr)

		// Init the rtph264-rtp-header-encoder only once,
		// and reuse if another rtmp-client connects.
		h264Encoder := rtph264.Encoder{
			PayloadType:    96,
			PayloadMaxSize: 1200,
		}
		h264Encoder.Init()

		rtmpServer.HandleConn = func(c *rtmp.Conn, nc net.Conn) {
			log.Println("New rtmp-conn created")

			sps := []byte{}
			pps := []byte{}

			videoJB, err := NewVideoJitterBuffer(videoTrack,
				time.Duration(jitterQueueLenMSFlag)*time.Millisecond)
			if err != nil {
				log.Fatalf("Failed to setup vjb: %s", err)
			}
			defer videoJB.Close()

			for {
				packet, err := c.ReadPacket()
				if err != nil {
					log.Println("Failed to read packet:", err)
					//rtmpTerminated <- true
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
						err = videoJB.WriteRTP(pkt)
						if err != nil {
							log.Printf("Failed to write h264 sample: %s", err)
							return
						}
					}
				}
			}
		}

		for {
			nc, err := lis.Accept()
			if err != nil {
				time.Sleep(time.Second)
				continue
			}
			log.Println("New Client connected")
			rtmpServer.HandleNetConn(nc)
		}

	}()
}
