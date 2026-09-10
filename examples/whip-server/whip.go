package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	ghost "github.com/eyeson-team/ghost/v2"
	"github.com/pion/interceptor"
	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v3"
	log "github.com/rs/zerolog/log"
)

// iceGatherTimeout bounds how long the answer waits for ice candidates. A turn
// server that is slow to allocate must not hold up a publish; host candidates
// are there almost immediately.
const iceGatherTimeout = 2 * time.Second

// ConnectFunc hands back the eyeson tracks to forward to, for a given video
// codec. It is called once per WHIP session, after the codec has been picked.
type ConnectFunc func(codec VideoCodec) (video ghost.RTPWriter, audio ghost.RTPWriter, err error)

// WHIPConfig holds everything the WHIP endpoint needs to know.
type WHIPConfig struct {
	// ListenAddr is the address the http server binds to, e.g. ":8100".
	ListenAddr string
	// Path is the http path senders publish to, e.g. "/whip".
	Path string
	// BearerToken, if not empty, is required in the Authorization header.
	BearerToken string
	// TLSCertFile and TLSKeyFile enable https when both are set.
	TLSCertFile string
	TLSKeyFile  string
	// ICE configures how the ingest peer connection is reached.
	ICE ICESettings
	// VideoCodecs is the server side codec preference, most wanted first. The
	// first entry the sender also offers wins.
	VideoCodecs []VideoCodec
	// PLIInterval defines how often a keyframe is requested from the sender.
	// Zero disables the periodic request.
	PLIInterval time.Duration
	// Connect provides the eyeson side tracks for the negotiated codec.
	Connect ConnectFunc
	// OnSessionEnded is called whenever an ingest session goes away.
	OnSessionEnded func()
}

// WHIPServer implements a minimal WHIP (WebRTC-HTTP Ingestion Protocol)
// endpoint. It accepts a single publishing session at a time and forwards the
// received RTP packets into an eyeson meeting.
type WHIPServer struct {
	cfg WHIPConfig

	mu      sync.Mutex
	session *whipSession
}

type whipSession struct {
	id    string
	pc    *webrtc.PeerConnection
	codec VideoCodec

	// ready is closed once the meeting connection has been established (or has
	// failed). Until then video and audio are nil and arriving packets are
	// dropped - the sender is already publishing at that point.
	ready chan struct{}

	video  ghost.RTPWriter
	audio  ghost.RTPWriter
	closed bool

	// A sender may publish more than one video track (OBS simulcast, multi
	// track WHIP). Only the first track per kind is forwarded, the rest is
	// read and dropped.
	videoTaken bool
	audioTaken bool
}

// NewWHIPServer creates a WHIP endpoint. Call Start to actually serve it.
func NewWHIPServer(cfg WHIPConfig) *WHIPServer {
	if !strings.HasPrefix(cfg.Path, "/") {
		cfg.Path = "/" + cfg.Path
	}
	cfg.Path = strings.TrimSuffix(cfg.Path, "/")
	if cfg.Path == "" {
		cfg.Path = "/whip"
	}
	return &WHIPServer{cfg: cfg}
}

// Start runs the http server in the background.
func (s *WHIPServer) Start() error {
	if s.cfg.Connect == nil {
		return errors.New("no connect function configured")
	}
	if len(s.cfg.VideoCodecs) == 0 {
		return errors.New("no video codecs configured")
	}

	mux := http.NewServeMux()
	mux.HandleFunc(s.cfg.Path, s.handleEndpoint)
	mux.HandleFunc(s.cfg.Path+"/", s.handleResource)

	server := &http.Server{Addr: s.cfg.ListenAddr, Handler: mux}

	scheme := "http"
	if s.cfg.TLSCertFile != "" && s.cfg.TLSKeyFile != "" {
		scheme = "https"
	}

	// Listen synchronously so a busy port is reported right away.
	listener, err := net.Listen("tcp", s.cfg.ListenAddr)
	if err != nil {
		return err
	}

	log.Info().Msgf("WHIP endpoint listening on %s://%s%s", scheme,
		s.cfg.ListenAddr, s.cfg.Path)

	go func() {
		var err error
		if scheme == "https" {
			err = server.ServeTLS(listener, s.cfg.TLSCertFile, s.cfg.TLSKeyFile)
		} else {
			err = server.Serve(listener)
		}
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error().Err(err).Msg("WHIP http server stopped")
			s.notifySessionEnded()
		}
	}()

	return nil
}

//
// http handlers
//

func (s *WHIPServer) handleEndpoint(w http.ResponseWriter, r *http.Request) {
	setCORSHeaders(w)

	switch r.Method {
	case http.MethodOptions:
		// Preflight and ICE server discovery.
		s.setICELinkHeaders(w)
		w.WriteHeader(http.StatusNoContent)
	case http.MethodPost:
		s.handlePublish(w, r)
	default:
		w.Header().Set("Allow", "POST, OPTIONS")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *WHIPServer) handleResource(w http.ResponseWriter, r *http.Request) {
	setCORSHeaders(w)

	resourceID := strings.TrimPrefix(r.URL.Path, s.cfg.Path+"/")

	switch r.Method {
	case http.MethodOptions:
		w.WriteHeader(http.StatusNoContent)
	case http.MethodDelete:
		if !s.closeSession(resourceID) {
			http.Error(w, "unknown resource", http.StatusNotFound)
			return
		}
		log.Info().Msgf("WHIP session %s deleted by sender", resourceID)
		w.WriteHeader(http.StatusOK)
	case http.MethodPatch:
		// Trickle ICE and ICE restart are optional in WHIP. This example
		// answers with the full set of candidates right away, so senders never
		// need to patch anything.
		http.Error(w, "trickle ice is not supported", http.StatusMethodNotAllowed)
	default:
		w.Header().Set("Allow", "DELETE, OPTIONS")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *WHIPServer) handlePublish(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		w.Header().Set("WWW-Authenticate", "Bearer")
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/sdp") {
		http.Error(w, "expected content-type application/sdp",
			http.StatusUnsupportedMediaType)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 256*1024))
	if err != nil || len(body) == 0 {
		http.Error(w, "missing sdp offer", http.StatusBadRequest)
		return
	}
	offer := string(body)

	// Pick the codec first: it decides how the eyeson side is connected and
	// which codec ends up in the answer.
	offeredVideo, err := OfferedVideoCodecs(offer)
	if err != nil {
		log.Warn().Err(err).Msg("Failed to parse the sdp offer")
		http.Error(w, "invalid sdp offer", http.StatusBadRequest)
		return
	}
	offeredAudio, err := OfferedAudioCodecs(offer)
	if err != nil {
		log.Warn().Err(err).Msg("Failed to parse the sdp offer")
		http.Error(w, "invalid sdp offer", http.StatusBadRequest)
		return
	}

	// Opus is the only audio codec that can be forwarded: the ghost audio track
	// is an Opus track and this example does not transcode. Anything else is
	// answered with a rejected audio section, so the session still comes up -
	// just without sound. Say so, otherwise it looks like a silent failure.
	hasAudio := len(offeredAudio) > 0
	forwardAudio := OffersOpus(offeredAudio)
	if hasAudio && !forwardAudio {
		log.Warn().Msgf("Sender offers audio as %v, only Opus can be forwarded. "+
			"Publishing without audio.", offeredAudio)
	}

	codec, ok := SelectVideoCodec(s.cfg.VideoCodecs, offeredVideo)
	switch {
	case ok:
		log.Info().Msgf("Sender offers video as %v, using %s", offeredVideo, codec.Name)
	case len(offeredVideo) == 0 && forwardAudio:
		// audio only sender. ghost always creates a video track, so a codec
		// still has to be picked - take the preferred one, it stays silent.
		codec = s.cfg.VideoCodecs[0]
		log.Info().Msg("Sender offers no video, publishing audio only")
	case len(offeredVideo) == 0:
		log.Warn().Msg("Sender offers neither a usable video codec nor Opus audio")
		http.Error(w, "no forwardable media on offer", http.StatusUnsupportedMediaType)
		return
	default:
		log.Warn().Msgf("Sender offers video as %v, none of which is enabled", offeredVideo)
		http.Error(w, "no common video codec", http.StatusUnsupportedMediaType)
		return
	}

	// Only one publisher at a time. A new offer takes over, which is what you
	// want when a sender crashed and reconnects.
	s.mu.Lock()
	previous := s.session
	s.mu.Unlock()
	if previous != nil {
		log.Info().Msgf("New WHIP offer received, dropping session %s", previous.id)
		s.closeSession(previous.id)
	}

	pc, err := s.newIngestPeerConnection(codec)
	if err != nil {
		log.Error().Err(err).Msg("Failed to create ingest peer connection")
		http.Error(w, "failed to create peer connection", http.StatusInternalServerError)
		return
	}

	sess := &whipSession{
		id:    newSessionID(),
		pc:    pc,
		codec: codec,
		ready: make(chan struct{}),
	}

	// Joining the meeting takes seconds, and the sender is waiting for this
	// http response - senders give up long before a cold ghost connect and a
	// full ICE gathering are done. So answer first and connect in parallel;
	// packets that arrive before the meeting is up are dropped.
	go s.connectMeeting(sess, forwardAudio)

	pc.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		log.Info().Msgf("WHIP track received: kind=%s codec=%s ssrc=%d",
			track.Kind(), track.Codec().MimeType, track.SSRC())
		s.forwardTrack(sess, track)
	})

	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		log.Info().Msgf("WHIP session %s connection state: %s", sess.id, state)
		switch state {
		case webrtc.PeerConnectionStateFailed,
			webrtc.PeerConnectionStateDisconnected,
			webrtc.PeerConnectionStateClosed:
			s.closeSession(sess.id)
		}
	})

	if err := pc.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeOffer,
		SDP:  offer,
	}); err != nil {
		log.Error().Err(err).Msg("Failed to set remote description")
		pc.Close()
		http.Error(w, "invalid sdp offer", http.StatusBadRequest)
		return
	}

	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		log.Error().Err(err).Msg("Failed to create answer")
		pc.Close()
		http.Error(w, "failed to create answer", http.StatusInternalServerError)
		return
	}

	// Gather candidates before replying, so no trickle ICE is needed. A TURN
	// server that is slow to allocate must not hold up the answer, so this is
	// bounded: pion keeps adding candidates to the local description as they
	// arrive, and host candidates are there almost immediately.
	gatherComplete := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(answer); err != nil {
		log.Error().Err(err).Msg("Failed to set local description")
		pc.Close()
		http.Error(w, "failed to set local description", http.StatusInternalServerError)
		return
	}

	gatherStart := time.Now()
	select {
	case <-gatherComplete:
		log.Debug().Msgf("ICE gathering finished in %s",
			time.Since(gatherStart).Round(time.Millisecond))
	case <-time.After(iceGatherTimeout):
		log.Debug().Msgf("ICE gathering still running after %s, answering with "+
			"the candidates gathered so far", iceGatherTimeout)
	}

	s.mu.Lock()
	s.session = sess
	s.mu.Unlock()

	w.Header().Set("Content-Type", "application/sdp")
	w.Header().Set("Location", s.resourceURL(r, sess.id))
	s.setICELinkHeaders(w)
	w.WriteHeader(http.StatusCreated)
	if _, err := w.Write([]byte(pc.LocalDescription().SDP)); err != nil {
		log.Warn().Err(err).Msg("Failed to write sdp answer")
	}

	log.Info().Msgf("WHIP session %s established", sess.id)
}

//
// webrtc plumbing
//

// newIngestPeerConnection builds the peer connection that faces the WHIP
// sender. Only the negotiated video codec and Opus are registered, which
// guarantees that whatever arrives here can be forwarded to eyeson untouched.
func (s *WHIPServer) newIngestPeerConnection(codec VideoCodec) (*webrtc.PeerConnection, error) {
	m := &webrtc.MediaEngine{}

	videoFeedback := []webrtc.RTCPFeedback{
		{Type: "nack"},
		{Type: "nack", Parameter: "pli"},
		{Type: "goog-remb"},
	}

	if err := m.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:     codec.MimeType,
			ClockRate:    90000,
			RTCPFeedback: videoFeedback,
		},
		PayloadType: 96,
	}, webrtc.RTPCodecTypeVideo); err != nil {
		return nil, err
	}

	if err := m.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:    webrtc.MimeTypeOpus,
			ClockRate:   48000,
			Channels:    2,
			SDPFmtpLine: "minptime=10;useinbandfec=1",
		},
		PayloadType: 111,
	}, webrtc.RTPCodecTypeAudio); err != nil {
		return nil, err
	}

	interceptorRegistry := &interceptor.Registry{}
	if err := webrtc.RegisterDefaultInterceptors(m, interceptorRegistry); err != nil {
		return nil, err
	}

	settingEngine, err := s.cfg.ICE.SettingEngine()
	if err != nil {
		return nil, err
	}

	api := webrtc.NewAPI(webrtc.WithMediaEngine(m),
		webrtc.WithInterceptorRegistry(interceptorRegistry),
		webrtc.WithSettingEngine(settingEngine))

	return api.NewPeerConnection(webrtc.Configuration{
		ICEServers: s.cfg.ICE.ICEServers(),
	})
}

// connectMeeting establishes the eyeson connection for a session in the
// background and publishes the tracks to it.
func (s *WHIPServer) connectMeeting(sess *whipSession, forwardAudio bool) {
	start := time.Now()
	video, audio, err := s.cfg.Connect(sess.codec)
	if err != nil {
		log.Error().Err(err).Msg("Failed to connect to the meeting, dropping the session")
		close(sess.ready)
		s.closeSession(sess.id)
		return
	}

	if !forwardAudio {
		audio = nil
	}

	s.mu.Lock()
	sess.video = video
	sess.audio = audio
	s.mu.Unlock()

	close(sess.ready)

	log.Info().Msgf("Meeting connection ready after %s, forwarding starts now",
		time.Since(start).Round(time.Millisecond))
}

// claimTarget returns the eyeson track this incoming track should be written
// to, or nil when there already is one of that kind. A sender may publish more
// than one video track (OBS simulcast, multi track WHIP); only the first per
// kind is forwarded.
func (s *WHIPServer) claimTarget(sess *whipSession, track *webrtc.TrackRemote) ghost.RTPWriter {
	s.mu.Lock()
	defer s.mu.Unlock()

	if track.Kind() == webrtc.RTPCodecTypeVideo {
		if sess.videoTaken {
			return nil
		}
		sess.videoTaken = true
		return sess.video
	}

	if sess.audioTaken {
		return nil
	}
	sess.audioTaken = true
	return sess.audio
}

// forwardTrack pumps the RTP packets of one incoming track into the matching
// eyeson track. Payload type and SSRC are rewritten by pion on write, so the
// payloads can be passed on as they are - no decoding, no re-encoding.
//
// The track is read from the moment it appears, even while the meeting
// connection is still coming up: not reading would stall the receiver and its
// RTCP. Those early packets are counted and dropped.
func (s *WHIPServer) forwardTrack(sess *whipSession, track *webrtc.TrackRemote) {
	var target ghost.RTPWriter
	claimed := false
	dropped := 0

	for {
		packet, _, err := track.ReadRTP()
		if err != nil {
			if !errors.Is(err, io.EOF) {
				log.Debug().Err(err).Msgf("Stopped reading %s track", track.Kind())
			}
			return
		}

		if !claimed {
			select {
			case <-sess.ready:
				claimed = true
				target = s.claimTarget(sess, track)
				if dropped > 0 {
					log.Debug().Msgf("Dropped %d %s packets while the meeting "+
						"connection was coming up", dropped, track.Kind())
				}
				if target == nil {
					log.Info().Msgf("Dropping %s track (rid %q), it is either "+
						"disabled or a second track of that kind",
						track.Kind(), track.RID())
				} else if track.Kind() == webrtc.RTPCodecTypeVideo {
					go s.requestKeyframes(sess.pc, track)
				}
			default:
				dropped++
				continue
			}
		}

		if target == nil {
			continue
		}

		// The header extensions belong to the WHIP negotiation (transport-cc,
		// abs-send-time, mid, rid). Their ids mean nothing on the eyeson side,
		// which does not negotiate extensions at all, so drop them.
		packet.Header.Extension = false
		packet.Header.ExtensionProfile = 0
		packet.Header.Extensions = nil

		if err := target.WriteRTP(packet); err != nil {
			if errors.Is(err, io.ErrClosedPipe) {
				// eyeson side is gone
				return
			}
			log.Warn().Err(err).Msgf("Failed to forward %s packet", track.Kind())
		}
	}
}

// requestKeyframes asks the sender for a fresh keyframe every PLIInterval.
// ghost does not surface the keyframe requests coming from the eyeson server,
// so a periodic request keeps late joiners from staring at a black tile.
func (s *WHIPServer) requestKeyframes(pc *webrtc.PeerConnection, track *webrtc.TrackRemote) {
	if s.cfg.PLIInterval <= 0 {
		return
	}
	ticker := time.NewTicker(s.cfg.PLIInterval)
	defer ticker.Stop()
	for range ticker.C {
		if err := pc.WriteRTCP([]rtcp.Packet{
			&rtcp.PictureLossIndication{MediaSSRC: uint32(track.SSRC())},
		}); err != nil {
			return
		}
	}
}

//
// helpers
//

func (s *WHIPServer) authorized(r *http.Request) bool {
	if s.cfg.BearerToken == "" {
		return true
	}
	header := r.Header.Get("Authorization")
	return header == "Bearer "+s.cfg.BearerToken
}

func (s *WHIPServer) resourceURL(r *http.Request, id string) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if forwarded := r.Header.Get("X-Forwarded-Proto"); forwarded != "" {
		scheme = forwarded
	}
	return fmt.Sprintf("%s://%s%s/%s", scheme, r.Host, s.cfg.Path, id)
}

// closeSession tears down the session with the given id. It reports whether a
// session was actually closed.
func (s *WHIPServer) closeSession(id string) bool {
	s.mu.Lock()
	sess := s.session
	if sess == nil || sess.id != id || sess.closed {
		s.mu.Unlock()
		return false
	}
	sess.closed = true
	s.session = nil
	s.mu.Unlock()

	if err := sess.pc.Close(); err != nil {
		log.Debug().Err(err).Msg("Failed to close ingest peer connection")
	}
	s.notifySessionEnded()
	return true
}

func (s *WHIPServer) notifySessionEnded() {
	if s.cfg.OnSessionEnded != nil {
		s.cfg.OnSessionEnded()
	}
}

// setICELinkHeaders advertises the STUN and TURN servers to the sender.
func (s *WHIPServer) setICELinkHeaders(w http.ResponseWriter) {
	for _, header := range s.cfg.ICE.LinkHeaders() {
		w.Header().Add("Link", header)
	}
}

// newSessionID returns the opaque id used in the resource url.
func newSessionID() string {
	buffer := make([]byte, 16)
	if _, err := rand.Read(buffer); err != nil {
		// crypto/rand does not fail in practice, and a session id only has to
		// be unguessable for as long as the session lives
		return hex.EncodeToString([]byte(time.Now().String()))
	}
	return hex.EncodeToString(buffer)
}

func setCORSHeaders(w http.ResponseWriter) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "POST, PATCH, DELETE, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
	w.Header().Set("Access-Control-Expose-Headers", "Location, Link")
}
