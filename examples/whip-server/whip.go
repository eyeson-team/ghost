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
	"github.com/pion/sdp/v3"
	"github.com/pion/webrtc/v3"
	log "github.com/rs/zerolog/log"
)

// sdesRepairRTPStreamIDURI is the header extension an rtx stream uses to name
// the simulcast layer it repairs, RFC 8852. Spelled out because the pinned
// pion/sdp has constants for mid and rid, but not yet for this one.
const sdesRepairRTPStreamIDURI = "urn:ietf:params:rtp-hdrext:sdes:repaired-rtp-stream-id"

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
	// Simulcast is SimulcastSelect or SimulcastDecline, see simulcast.go.
	Simulcast string
	// SimulcastRID selects the simulcast layer to forward by its rid. Empty or
	// "auto" picks the layer with the highest bitrate.
	SimulcastRID string
	// Connect provides the eyeson side tracks for the negotiated codec.
	Connect ConnectFunc
	// OnSessionEnded is called whenever an ingest session goes away.
	OnSessionEnded func()
	// OnSessionState, if set, is called with every state the ingest peer
	// connection reaches. OnSessionEnded says that a session is over,
	// this says how it was doing while it lasted - connected above all,
	// which is the first moment a sender is known to have reached us.
	OnSessionState func(id string, state webrtc.PeerConnectionState)
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

	// A sender may publish more than one video track (multi track WHIP).
	// Only the first track per kind is forwarded, the rest is read and
	// dropped. Simulcast layers go through the selector first, so the first
	// video track to claim a target is the chosen layer.
	videoTaken bool
	audioTaken bool

	simulcast *simulcastSelector
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

	// logRequests writes one debug line per request. A sender whose PATCH or
	// DELETE was turned down looks exactly like a sender that never called at
	// all without it, see diagnostics.go.
	server := &http.Server{Addr: s.cfg.ListenAddr, Handler: logRequests(mux)}

	scheme := "http"
	if s.cfg.TLSCertFile != "" && s.cfg.TLSKeyFile != "" {
		scheme = "https"
	}

	// Listen synchronously so a busy port is reported right away.
	listener, err := net.Listen("tcp", s.cfg.ListenAddr)
	if err != nil {
		return err
	}

	// A sender on another machine needs an address it can reach, and ":8100"
	// is not one, so every local address is spelled out, see netinfo.go.
	for _, endpoint := range EndpointURLs(scheme, s.cfg.ListenAddr, s.cfg.Path) {
		log.Info().Msgf("WHIP endpoint listening on %s", endpoint)
	}

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
		// Trickle ICE. This server answers with its own candidates right away,
		// so nothing has to be patched in that direction - but a sender whose
		// offer carries no candidates has no other way to say where its media
		// arrives, and an ice-lite sender never sends checks that would let us
		// find out. See trickle.go.
		s.handleTrickle(w, r, resourceID)
	default:
		w.Header().Set("Allow", "DELETE, PATCH, OPTIONS")
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

	logSDP("Offer from the WHIP sender", offer)

	if s.cfg.Simulcast == SimulcastDecline {
		if declined, found := DeclineSimulcast(offer); found {
			log.Info().Msg("The sender offers simulcast, answering without it " +
				"so it sends a single stream. A sender that refuses that " +
				"(OBS: \"accepted 0 simulcast layers\") has to be set to one " +
				"layer, or this server run with --simulcast select")
			offer = declined
		}
	}

	// An offer without candidates is not an error - they may still be trickled
	// in - but if they never arrive the session just times out half a minute
	// later, with nothing in the log that points at the cause. The summary also
	// decides whether this session has to be answered as a lite agent.
	offerICE := LogOfferICE(offer)

	// Pick the codec first: it decides how the eyeson side is connected and
	// which codec ends up in the answer. Formats are compared, not just codec
	// names, because a sender may offer the same codec in a flavour the meeting
	// server cannot decode - VP9 in a profile other than 0.
	offeredVideo, err := OfferedVideoFormats(offer)
	if err != nil {
		log.Warn().Err(err).Msg("Failed to parse the sdp offer")
		http.Error(w, "invalid sdp offer", http.StatusBadRequest)
		return
	}
	offeredVideoCodecs := VideoMimeTypes(offeredVideo)
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

	// A codec that is offered but only in an unusable format looks exactly like
	// a codec that was never offered, so say what happened to it.
	for _, unusable := range UnusableVideoFormats(s.cfg.VideoCodecs, offeredVideo) {
		log.Warn().Msgf("Ignoring %s, the meeting server cannot decode that format",
			unusable)
	}

	codec, ok := SelectVideoCodec(s.cfg.VideoCodecs, offeredVideo)
	switch {
	case ok:
		log.Info().Msgf("Sender offers video as %v, using %s", offeredVideoCodecs, codec.Name)
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
		log.Warn().Msgf("Sender offers video as %v, none of which is usable",
			offeredVideoCodecs)
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

	pc, err := s.newIngestPeerConnection(codec, offerICE.NeedsLiteAnswer())
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

		simulcast: newSimulcastSelector(s.cfg.SimulcastRID),
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

	// The connection state says that something failed, the ice state and the
	// selected pair say where it got stuck. See diagnostics.go.
	LogSessionICE(sess.id, pc)

	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		log.Info().Msgf("WHIP session %s connection state: %s", sess.id, state)
		if s.cfg.OnSessionState != nil {
			s.cfg.OnSessionState(sess.id, state)
		}
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

	answerSDP := pc.LocalDescription().SDP
	logSDP("Answer to the WHIP sender", answerSDP)

	w.Header().Set("Content-Type", "application/sdp")
	w.Header().Set("Location", s.resourceURL(r, sess.id))
	// RFC 9725 section 4.2: a resource that takes trickled candidates carries
	// an ETag, and some senders only try a PATCH once they have seen one.
	w.Header().Set("ETag", `"`+sess.id+`"`)
	s.setICELinkHeaders(w)
	w.WriteHeader(http.StatusCreated)
	if _, err := w.Write([]byte(answerSDP)); err != nil {
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
//
// lite answers this one session as an ice lite agent, even when the server was
// not started with --ice-lite: the offer left no other way to connect. See
// OfferICE.NeedsLiteAnswer.
func (s *WHIPServer) newIngestPeerConnection(codec VideoCodec, lite bool) (*webrtc.PeerConnection, error) {
	m := &webrtc.MediaEngine{}

	videoFeedback := []webrtc.RTCPFeedback{
		{Type: "nack"},
		{Type: "nack", Parameter: "pli"},
		{Type: "goog-remb"},
	}

	// The fmtp line is part of the match: with "profile-id=0" registered, a
	// sender that offers VP9 twice (Chrome offers profile 0 and profile 2) is
	// answered with the profile 0 payload type only, and the answer carries the
	// fmtp line so the sender knows which one to use.
	if err := m.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:     codec.MimeType,
			ClockRate:    90000,
			SDPFmtpLine:  codec.FmtpLine,
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

	// Simulcast layers share one m-line and are told apart by the mid and rid
	// header extensions. Without them in the answer the sender still sends
	// every layer, but pion cannot map the ssrcs to the transceiver and drops
	// them ("mid RTP Extensions required for Simulcast"). OBS offers both.
	// The repaired rid is what an rtx stream of a layer would carry.
	for _, extension := range []struct {
		uri  string
		kind webrtc.RTPCodecType
	}{
		{sdp.SDESMidURI, webrtc.RTPCodecTypeVideo},
		{sdp.SDESRTPStreamIDURI, webrtc.RTPCodecTypeVideo},
		{sdesRepairRTPStreamIDURI, webrtc.RTPCodecTypeVideo},
		{sdp.SDESMidURI, webrtc.RTPCodecTypeAudio},
	} {
		if err := m.RegisterHeaderExtension(webrtc.RTPHeaderExtensionCapability{
			URI: extension.uri,
		}, extension.kind); err != nil {
			return nil, err
		}
	}

	interceptorRegistry := &interceptor.Registry{}
	if err := webrtc.RegisterDefaultInterceptors(m, interceptorRegistry); err != nil {
		return nil, err
	}

	ice := s.cfg.ICE
	if lite {
		ice.Lite = true
	}

	settingEngine, err := ice.SettingEngine()
	if err != nil {
		return nil, err
	}

	api := webrtc.NewAPI(webrtc.WithMediaEngine(m),
		webrtc.WithInterceptorRegistry(interceptorRegistry),
		webrtc.WithSettingEngine(settingEngine))

	// A lite agent gathers host candidates only, so stun and turn would just
	// hold the answer up for nothing.
	iceServers := ice.ICEServers()
	if ice.Lite {
		iceServers = nil
	}

	return api.NewPeerConnection(webrtc.Configuration{
		ICEServers: iceServers,
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
// than one video track (multi track WHIP); only the first per kind is
// forwarded. Simulcast layers only get here once they have been chosen.
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
	// A video track with a rid is one layer of a simulcast sender. All layers
	// are measured, one is forwarded, see simulcast.go.
	var layer *simulcastLayer
	if track.Kind() == webrtc.RTPCodecTypeVideo && track.RID() != "" {
		layer = sess.simulcast.add(track)
	}

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

		if layer != nil {
			layer.bytes.Add(uint64(len(packet.Payload)))
		}

		if !claimed {
			select {
			case <-sess.ready:
				if layer != nil && !sess.simulcast.isChosen(layer) {
					// not decided yet, or another layer won
					continue
				}
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
//
// Forwarding usually starts in the middle of a group of pictures - always so for
// a simulcast layer, which is only picked after it has been running for a
// while - so one keyframe is asked for right away, whatever the interval.
func (s *WHIPServer) requestKeyframes(pc *webrtc.PeerConnection, track *webrtc.TrackRemote) {
	pli := []rtcp.Packet{
		&rtcp.PictureLossIndication{MediaSSRC: uint32(track.SSRC())},
	}
	if err := pc.WriteRTCP(pli); err != nil {
		return
	}
	if s.cfg.PLIInterval <= 0 {
		return
	}
	ticker := time.NewTicker(s.cfg.PLIInterval)
	defer ticker.Stop()
	for range ticker.C {
		if err := pc.WriteRTCP(pli); err != nil {
			return
		}
	}
}

//
// helpers
//

// logSDP writes one side of the WHIP handshake to the trace log. The session
// description is printed as an indented block: it is the one log message where
// the line breaks carry the meaning.
//
// This is trace rather than debug because it is a raw protocol dump, and
// because nothing is built when the level is off - it runs on every publish and
// an offer with all its candidates is a few kilobytes.
func logSDP(what, sdp string) {
	event := log.Trace()
	if !event.Enabled() {
		return
	}

	lines := strings.Split(strings.ReplaceAll(sdp, "\r\n", "\n"), "\n")
	block := make([]string, 0, len(lines)+1)
	block = append(block, "")
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		block = append(block, "  "+line)
	}

	event.Msgf("%s:%s", what, strings.Join(block, "\n"))
}

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