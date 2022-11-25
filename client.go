package ghost

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/eyeson-team/gosepp/v3"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v3"
)

// StdoutLogger simple logger logging everything to stdout
type StdoutLogger struct{}

// Error log error msg
func (sl *StdoutLogger) Error(format string, v ...interface{}) {
	fmt.Printf("GHOST [ERROR] %s\n", fmt.Sprintf(format, v...))
}

// Warn log warn message
func (sl *StdoutLogger) Warn(format string, v ...interface{}) {
	fmt.Printf("GHOST [WARN] %s\n", fmt.Sprintf(format, v...))
}

// Info log info message
func (sl *StdoutLogger) Info(format string, v ...interface{}) {
	fmt.Printf("GHOST [INFO] %s\n", fmt.Sprintf(format, v...))
}

// Debug log debug message
func (sl *StdoutLogger) Debug(format string, v ...interface{}) {
	fmt.Printf("GHOST [DEBUG] %s\n", fmt.Sprintf(format, v...))
}

// Trace log trace message
func (sl *StdoutLogger) Trace(format string, v ...interface{}) {
	fmt.Printf("GHOST [TRACE] %s\n", fmt.Sprintf(format, v...))
}

// RTPWriter interface  which is implemented by video and audio tracks
type RTPWriter interface {
	WriteRTP(p *rtp.Packet) error
}

// ConnectedHandler called when connection succeeded. Providing tracks to write to.
type ConnectedHandler func(connected bool, localVideoTrack RTPWriter, localAudioTrack RTPWriter)

// TerminatedHandler handle function call when call is terminated
type TerminatedHandler func()

// DataChannelReceivedHandler delegate for the data channel
type DataChannelReceivedHandler func(data []byte)

// EyesonClient call interface
type EyesonClient interface {
	Call() error
	TerminateCall() error
	Destroy()
	SetConnectedHandler(ConnectedHandler)
	SetTerminatedHandler(TerminatedHandler)
	SetDataChannelHandler(DataChannelReceivedHandler)
}

// ClientConfigInterface extends the gosepp CallInfoInterface with methods to
// get STUN and TURN-servers.
type ClientConfigInterface interface {
	gosepp.CallInfoInterface

	GetStunServers() []string
	GetTurnServerURLs() []string
	GetTurnServerPassword() string
	GetTurnServerUsername() string
	GetDisplayname() string
}

// Client implements the EyesonClient call interface
type Client struct {
	callInfo                   ClientConfigInterface
	clientID                   string
	confID                     string
	peerConnection             *webrtc.PeerConnection
	call                       *gosepp.Call
	callID                     string
	sfuCapable                 bool
	sendPong                   bool
	sendOnly                   bool
	useH264Codec               bool
	useConfProtocol            bool
	sendMessagesViaSEPP        bool
	connectedHandler           ConnectedHandler
	terminatedHandler          TerminatedHandler
	dataChannelReceivedHandler DataChannelReceivedHandler
	logger                     gosepp.Logger
}

// ClientOption following options pattern to specify options
// for the client.
type ClientOption func(*Client)

// WithSendOnly signals the only outbound (client->server)
// traffic is wanted
func WithSendOnly() ClientOption {
	return func(h *Client) {
		h.sendOnly = true
	}
}

// WithForceH264Codec forces the h264 codec.
func WithForceH264Codec() ClientOption {
	return func(h *Client) {
		h.useH264Codec = true
	}
}

// WithNoConfProtocol deactivates the confserver
// protocol. So no additional infos will be sent.
func WithNoConfProtocol() ClientOption {
	return func(h *Client) {
		h.useConfProtocol = false
	}
}

// WithNoSFUSupport configures that no SFU
// support should be used.
func WithNoSFUSupport() ClientOption {
	return func(h *Client) {
		h.sfuCapable = false
	}
}

// WithCustomLogger configures a custom logger.
func WithCustomLogger(logger gosepp.Logger) ClientOption {
	return func(h *Client) {
		h.logger = logger
	}
}

// NewClient creates a new ghost client.
func NewClient(callInfo ClientConfigInterface, opts ...ClientOption) (EyesonClient, error) {

	cl := &Client{
		callInfo:            callInfo,
		clientID:            callInfo.GetClientID(),
		confID:              callInfo.GetConfID(),
		sfuCapable:          true,
		sendPong:            true,
		sendOnly:            false,
		useH264Codec:        false,
		useConfProtocol:     true,
		sendMessagesViaSEPP: true,
		logger:              &StdoutLogger{},
	}

	for _, opt := range opts {
		opt(cl)
	}

	if err := cl.initStack(cl.useH264Codec); err != nil {
		return nil, err
	}
	if err := cl.initSig(); err != nil {
		return nil, err
	}
	return cl, nil
}

// Destroy destroyes a client and closes call and peer connection.
func (cl *Client) Destroy() {
	if cl.call != nil {
		cl.call.Close()
	}
	if cl.peerConnection != nil {
		cl.peerConnection.Close()
	}
}

// SetConnectedHandler forwards a listener callback to receive connection
// status updates.
func (cl *Client) SetConnectedHandler(handler ConnectedHandler) {
	cl.connectedHandler = handler
}

// SetTerminatedHandler forwards a listener callback to receive termination
// status updates.
func (cl *Client) SetTerminatedHandler(handler TerminatedHandler) {
	cl.terminatedHandler = handler
}

// SetDataChannelHandler forwards data received via data-channel.
func (cl *Client) SetDataChannelHandler(handler DataChannelReceivedHandler) {
	cl.dataChannelReceivedHandler = handler
}

// Call initiates a connection.
func (cl *Client) Call() error {
	// create our offer
	offer, err := cl.createOffer()
	if err != nil {
		return err
	}

	_, sdpAnswer, err := cl.call.Start(context.Background(),
		gosepp.Sdp{SdpType: "offer", Sdp: offer}, cl.callInfo.GetDisplayname())
	if err != nil {
		return err
	}

	if err := cl.peerConnection.SetRemoteDescription(
		webrtc.SessionDescription{SDP: sdpAnswer.Sdp, Type: webrtc.SDPTypeAnswer}); err != nil {
		cl.logger.Warn("Failed to set remote description: %s.\n", err)
	}

	return err
}

// TerminateCall requests to stop a call.
func (cl *Client) TerminateCall() error {
	return cl.call.Terminate(context.Background())
}

func (cl *Client) initSig() error {
	call, err := gosepp.NewCall(cl.callInfo, cl.logger)
	if err != nil {
		return err
	}
	cl.call = call

	call.SetSDPUpdateHandler(func(sdp gosepp.Sdp) {
		onSdpUpdate(call, cl.peerConnection, sdp, cl.logger)
	})

	call.SetTerminatedHandler(func() {
		if cl.terminatedHandler != nil {
			cl.terminatedHandler()
		}
	})

	return nil
}

func (cl *Client) initStack(useH264Codec bool) error {

	// Create a MediaEngine object to configure the supported codec
	m := webrtc.MediaEngine{}

	vCodecFBs := []webrtc.RTCPFeedback{
		webrtc.RTCPFeedback{Type: "nack"},
		webrtc.RTCPFeedback{Type: "nack", Parameter: "pli"},
		webrtc.RTCPFeedback{Type: "goog-remb"},
	}

	videoMimeType := webrtc.MimeTypeVP8
	if useH264Codec {
		videoMimeType = webrtc.MimeTypeH264
	}

	videoCaps := webrtc.RTPCodecCapability{MimeType: videoMimeType, ClockRate: 90000,
		Channels: 0, SDPFmtpLine: "", RTCPFeedback: vCodecFBs}

	if err := m.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: videoCaps,
		PayloadType:        96,
	}, webrtc.RTPCodecTypeVideo); err != nil {
		return err
	}

	opusCaps := webrtc.RTPCodecCapability{MimeType: "audio/opus", ClockRate: 48000,
		Channels: 0, SDPFmtpLine: "", RTCPFeedback: nil}

	if err := m.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: opusCaps,
		PayloadType:        111,
	}, webrtc.RTPCodecTypeAudio); err != nil {
		return err
	}

	// Create the API object with the MediaEngine
	api := webrtc.NewAPI(webrtc.WithMediaEngine(&m))

	// Prepare the configuration
	config := webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			{
				URLs: cl.callInfo.GetStunServers(),
			},
			{
				URLs:       cl.callInfo.GetTurnServerURLs(),
				Username:   cl.callInfo.GetTurnServerUsername(),
				Credential: cl.callInfo.GetTurnServerPassword(),
			},
		},
	}

	// Create a new RTCPeerConnection
	peerConnection, err := api.NewPeerConnection(config)
	if err != nil {
		return err
	}

	peerConnection.OnNegotiationNeeded(func() {
		//log.Println("Negotiation needed")
	})

	videoTrack, videoTrackErr := webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{MimeType: videoMimeType}, "video", "pion")
	if videoTrackErr != nil {
		return videoTrackErr
	}

	_, videoTrackErr = peerConnection.AddTrack(videoTrack)
	if videoTrackErr != nil {
		return videoTrackErr
	}

	audioTrack, audioTrackErr := webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{MimeType: "audio/opus"}, "audio", "pion")
	if audioTrackErr != nil {
		return audioTrackErr
	}

	_, audioTrackErr = peerConnection.AddTrack(audioTrack)
	if audioTrackErr != nil {
		return audioTrackErr
	}

	// Set the handler for ICE connection state
	// This will notify you when the peer has connected/disconnected
	peerConnection.OnICEConnectionStateChange(func(connectionState webrtc.ICEConnectionState) {
		cl.logger.Info("ICE Connection State has changed: %s\n", connectionState.String())
		switch connectionState {
		case webrtc.ICEConnectionStateConnected:
			if cl.connectedHandler != nil {
				cl.connectedHandler(true, videoTrack, audioTrack)
			}
		}
	})

	peerConnection.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		//fmt.Printf("onTrack: new track: id: %s mid: %s rid: %s codec: %s\n", track.ID(),
		//  track.Msid(), track.RID(), track.Codec().MimeType)

		// Without sending something over, the NO-DATA-RECEIVED will be triggered
		// serverside (although this should not, cause STUN-binding messages should trigger
		// this as well). Therefore send PLIs cyclically.
		codec := track.Codec()
		if strings.EqualFold(codec.MimeType, webrtc.MimeTypeH264) ||
			strings.EqualFold(codec.MimeType, webrtc.MimeTypeVP8) {
			go func() {
				pliTicker := time.NewTicker(10 * time.Second)
				defer pliTicker.Stop()
				for {
					select {
					case <-pliTicker.C:
						errSend := peerConnection.WriteRTCP(
							[]rtcp.Packet{&rtcp.PictureLossIndication{MediaSSRC: uint32(track.SSRC())}})
						if errSend != nil {
							//log.Println("Failed to send PLI. Stopping")
							return
						}
					}
				}
			}()
		}
	})

	var dataChannel *webrtc.DataChannel

	// Note: only use data-channel of remote.
	hasDataChannel := true
	if hasDataChannel {
		// Create a dummy data-channel. The remote's channel
		// well be setup via OnDataChannel (see next)
		negotiated := true
		var identifier uint16 = 0
		dataChannel, err = peerConnection.CreateDataChannel("data",
			&webrtc.DataChannelInit{Negotiated: &negotiated, ID: &identifier})
		if err != nil {
			return err
		}

		// Register channel opening handling
		dataChannel.OnOpen(func() {
			//log.Printf("Data channel '%s'-'%d' open.\n", dataChannel.Label(), dataChannel.ID())
		})

		// Register text message handling
		dataChannel.OnMessage(func(msg webrtc.DataChannelMessage) {
			//log.Printf("Message from DataChannel '%s': '%s'\n", dataChannel.Label(), string(msg.Data))

			type base struct {
				MsgType string `json:"type"`
			}

			b := base{}
			if err := json.Unmarshal(msg.Data, &b); err != nil {
				cl.logger.Warn("Failed to unmarshal: %s\n", err)
			}
			if b.MsgType == "ping" {
				//fmt.Println("Ping received")
				if cl.sendPong {
					// sending pong
					pong := base{MsgType: "pong"}
					b, _ := json.Marshal(pong)
					dataChannel.Send(b)
				}
			}

			if cl.dataChannelReceivedHandler != nil {
				cl.dataChannelReceivedHandler(msg.Data)
			}

		})
	}

	cl.peerConnection = peerConnection

	return nil
}

func (cl *Client) createOffer() (string, error) {
	offer, err := cl.peerConnection.CreateOffer(nil)
	if err != nil {
		return "", err
	}

	// wait until ice candidates are all ready
	gatherComplete := webrtc.GatheringCompletePromise(cl.peerConnection)

	// Sets the LocalDescription, and starts our UDP listeners
	err = cl.peerConnection.SetLocalDescription(offer)
	if err != nil {
		return "", err
	}

	<-gatherComplete

	newOffer := cl.peerConnection.LocalDescription().SDP

	// add session attribute `sfu-capable` to our offer
	// find first m line
	if cl.sfuCapable {
		firstMLineIdx := strings.Index(newOffer, "m=")
		if firstMLineIdx >= 0 {
			newOffer = newOffer[:firstMLineIdx] + "a=sfu-capable\r\n" + newOffer[firstMLineIdx:]
		}
	}

	if cl.useConfProtocol {
		// add session attribute `eyeson-datachan-capable` and `eyeson-datachan-keepalive`
		firstMLineIdx := strings.Index(newOffer, "m=")
		if firstMLineIdx >= 0 {
			sAttribs := []string{
				"a=eyeson-datachan-capable",
				"a=eyeson-datachan-keepalive",
			}
			if cl.sendMessagesViaSEPP {
				sAttribs = append(sAttribs, "a=eyeson-sepp-messaging")
			}
			newOffer = newOffer[:firstMLineIdx] + strings.Join(sAttribs, "\r\n") + "\r\n" + newOffer[firstMLineIdx:]
		}
	}

	if cl.sendOnly {
		// modify sendrecv -> sendonly
		newOffer = strings.ReplaceAll(newOffer, "a=sendrecv", "a=sendonly")
	}

	return newOffer, nil
}

func onSdpUpdate(call *gosepp.Call, pc *webrtc.PeerConnection, sdp gosepp.Sdp,
	logger gosepp.Logger) {
	switch sdp.SdpType {
	case "offer":
		offer := webrtc.SessionDescription{
			Type: webrtc.SDPTypeOffer,
			SDP:  sdp.Sdp,
		}

		err := pc.SetRemoteDescription(offer)
		if err != nil {
			logger.Warn("Failed to set remote description: %s", err)
			return
		}

		// create Answer
		answer, err := pc.CreateAnswer(nil)
		if err != nil {
			logger.Warn("Failed to create answer: %s", err)
			return
		}

		// Sets the LocalDescription, and starts our UDP listeners
		err = pc.SetLocalDescription(answer)
		if err != nil {
			logger.Warn("Failed to set local description: %s", err)
			return
		}

		if err = call.UpdateSDP(context.Background(),
			gosepp.Sdp{SdpType: "answer", Sdp: answer.SDP}); err != nil {
			logger.Warn("failed to send message:", err)
			return
		}
	}
}
