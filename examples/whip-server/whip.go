package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
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

// RFC 8852, missing in pion/sdp.
const sdesRepairRTPStreamIDURI = "urn:ietf:params:rtp-hdrext:sdes:repaired-rtp-stream-id"

const iceGatherTimeout = 2 * time.Second

type ConnectFunc func(codec VideoCodec) (video ghost.RTPWriter, audio ghost.RTPWriter, err error)

type WHIPConfig struct {
	ListenAddr     string
	Path           string
	BearerToken    string
	TLSCertFile    string
	TLSKeyFile     string
	ICE            ICESettings
	VideoCodecs    []VideoCodec
	PLIInterval    time.Duration
	Simulcast      string
	SimulcastRID   string
	Connect        ConnectFunc
	OnSessionEnded func()
	OnSessionState func(id string, state webrtc.PeerConnectionState)
}

type WHIPServer struct {
	cfg WHIPConfig

	mu      sync.Mutex
	session *whipSession
}

type whipSession struct {
	id    string
	pc    *webrtc.PeerConnection
	codec VideoCodec

	ready chan struct{}

	video  ghost.RTPWriter
	audio  ghost.RTPWriter
	closed bool

	videoTaken bool
	audioTaken bool

	simulcast *simulcastSelector
}

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

	server := &http.Server{Addr: s.cfg.ListenAddr, Handler: logRequests(mux)}

	scheme := "http"
	if s.cfg.TLSCertFile != "" && s.cfg.TLSKeyFile != "" {
		scheme = "https"
	}

	listener, err := net.Listen("tcp", s.cfg.ListenAddr)
	if err != nil {
		return err
	}

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

func (s *WHIPServer) handleEndpoint(w http.ResponseWriter, r *http.Request) {
	setCORSHeaders(w)

	switch r.Method {
	case http.MethodOptions:
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

	offerICE := LogOfferICE(offer)

	// VP9 may come in an unsupported profile.
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

	hasAudio := len(offeredAudio) > 0
	forwardAudio := OffersOpus(offeredAudio)
	if hasAudio && !forwardAudio {
		log.Warn().Msgf("Sender offers audio as %v, only Opus can be forwarded. "+
			"Publishing without audio.", offeredAudio)
	}

	for _, unusable := range UnusableVideoFormats(s.cfg.VideoCodecs, offeredVideo) {
		log.Warn().Msgf("Ignoring %s, the meeting server cannot decode that format",
			unusable)
	}

	codec, ok := SelectVideoCodec(s.cfg.VideoCodecs, offeredVideo)
	switch {
	case ok:
		log.Info().Msgf("Sender offers video as %v, using %s", offeredVideoCodecs, codec.Name)
	case len(offeredVideo) == 0 && forwardAudio:
		// ghost always needs a video track.
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

	// Senders time out before a cold meeting connect.
	go s.connectMeeting(sess, forwardAudio)

	pc.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		log.Info().Msgf("WHIP track received: kind=%s codec=%s ssrc=%d",
			track.Kind(), track.Codec().MimeType, track.SSRC())
		s.forwardTrack(sess, track)
	})

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

	// A slow TURN allocation must not delay the answer.
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
	// Some senders only try the first host candidate.
	answerSDP = preferHostCandidate(answerSDP, requestLocalIP(r))
	logSDP("Answer to the WHIP sender", answerSDP)

	w.Header().Set("Content-Type", "application/sdp")
	// Chunked answers (Go, over 2 KB) break FFmpeg.
	w.Header().Set("Content-Length", strconv.Itoa(len(answerSDP)))
	w.Header().Set("Location", s.resourceURL(r, sess.id))
	// RFC 9725 4.2: some senders only PATCH after an ETag.
	w.Header().Set("ETag", `"`+sess.id+`"`)
	s.setICELinkHeaders(w)
	w.WriteHeader(http.StatusCreated)
	if _, err := w.Write([]byte(answerSDP)); err != nil {
		log.Warn().Err(err).Msg("Failed to write sdp answer")
	}

	log.Info().Msgf("WHIP session %s established", sess.id)
}

func (s *WHIPServer) newIngestPeerConnection(codec VideoCodec, lite bool) (*webrtc.PeerConnection, error) {
	m := &webrtc.MediaEngine{}

	videoFeedback := []webrtc.RTCPFeedback{
		{Type: "nack"},
		{Type: "nack", Parameter: "pli"},
		{Type: "goog-remb"},
	}

	// Pins VP9 to profile 0.
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

	// pion drops simulcast layers without mid and rid.
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

	iceServers := ice.ICEServers()
	if ice.Lite {
		iceServers = nil
	}

	return api.NewPeerConnection(webrtc.Configuration{
		ICEServers: iceServers,
	})
}

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

	s.mu.Lock()
	closed := sess.closed
	s.mu.Unlock()

	close(sess.ready)

	if closed {
		log.Info().Msgf("Meeting connection ready after %s, but WHIP session %s "+
			"already ended, keeping the connection for the next sender",
			time.Since(start).Round(time.Millisecond), sess.id)
		return
	}

	log.Info().Msgf("Meeting connection ready after %s, forwarding starts now",
		time.Since(start).Round(time.Millisecond))
}

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

// Reads before the meeting is up too, or RTCP stalls.
func (s *WHIPServer) forwardTrack(sess *whipSession, track *webrtc.TrackRemote) {
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

		// Extension ids are only valid on the WHIP side.
		packet.Header.Extension = false
		packet.Header.ExtensionProfile = 0
		packet.Header.Extensions = nil

		if err := target.WriteRTP(packet); err != nil {
			if errors.Is(err, io.ErrClosedPipe) {
				return
			}
			log.Warn().Err(err).Msgf("Failed to forward %s packet", track.Kind())
		}
	}
}

// ghost does not surface eyeson's keyframe requests.
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

func (s *WHIPServer) setICELinkHeaders(w http.ResponseWriter) {
	for _, header := range s.cfg.ICE.LinkHeaders() {
		w.Header().Add("Link", header)
	}
}

func newSessionID() string {
	buffer := make([]byte, 16)
	if _, err := rand.Read(buffer); err != nil {
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

// nil for loopback, pion gathers no loopback candidates.
func requestLocalIP(r *http.Request) net.IP {
	addr, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr)
	if !ok {
		return nil
	}
	tcpAddr, ok := addr.(*net.TCPAddr)
	if !ok || tcpAddr.IP.IsLoopback() {
		return nil
	}
	return tcpAddr.IP
}

// Moves the host candidate on preferIP, else the first IPv4 one, to the front.
// Only line order changes, priorities stay.
func preferHostCandidate(sdpText string, preferIP net.IP) string {
	lines := strings.Split(strings.TrimRight(sdpText, "\r\n"), "\r\n")

	first, best, bestScore := -1, -1, 0
	for i, line := range lines {
		if !strings.HasPrefix(line, "a=candidate:") {
			continue
		}
		if first < 0 {
			first = i
		}
		fields := strings.Fields(line)
		if len(fields) < 8 || fields[6] != "typ" || fields[7] != "host" ||
			!strings.EqualFold(fields[2], "udp") {
			continue
		}
		ip := net.ParseIP(fields[4])
		score := 1
		if ip != nil && ip.To4() != nil {
			score = 2
		}
		if ip != nil && preferIP != nil && ip.Equal(preferIP) {
			score = 3
		}
		if score > bestScore {
			best, bestScore = i, score
		}
	}
	if first < 0 || best <= first {
		return sdpText
	}

	chosen := strings.Fields(lines[best])
	var picked, rest []string
	count := 0
	for i := first; i < len(lines) && strings.HasPrefix(lines[i], "a=candidate:"); i++ {
		fields := strings.Fields(lines[i])
		if len(fields) > 5 && fields[0] == chosen[0] &&
			fields[4] == chosen[4] && fields[5] == chosen[5] {
			picked = append(picked, lines[i])
		} else {
			rest = append(rest, lines[i])
		}
		count++
	}

	out := make([]string, 0, len(lines))
	out = append(out, lines[:first]...)
	out = append(out, picked...)
	out = append(out, rest...)
	out = append(out, lines[first+count:]...)
	return strings.Join(out, "\r\n") + "\r\n"
}