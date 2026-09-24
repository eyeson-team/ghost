package main

import (
	"fmt"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	ghost "github.com/eyeson-team/ghost/v2"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v3"
	log "github.com/rs/zerolog/log"
)

// dryRunReportInterval is how often the media counters are printed while a
// sender is connected. Long enough to stay out of the way, short enough to see
// that packets are still arriving.
const dryRunReportInterval = 5 * time.Second

// runDryRun serves the WHIP endpoint on its own: it prints the addresses a
// sender can be pointed at, then reports every session that arrives, but it
// never joins a meeting and throws the incoming media away.
//
// The point is the order of things. Setting up a streaming device takes longer
// than a meeting stays open with nobody in it, so the endpoint has to exist
// before the meeting does. In a dry run the sender can be configured, pointed at
// an address and started once, and whether it reaches this machine is answered
// right there - without an api key, without a meeting, and without anything
// that could time out while the device is still being set up.
//
// Everything in front of the meeting works exactly as it does in a real run:
// the same http endpoint with the same bearer token, the same codec
// negotiation and the same ice settings. So a sender that connects here
// connects later, and a codec that is turned down here is turned down later.
func runDryRun() {
	codecs, err := ParseVideoCodecs(videoCodecsFlag)
	if err != nil {
		log.Error().Err(err).Msg("Invalid --video-codecs")
		return
	}

	simulcastMode, err := ParseSimulcastMode(simulcastFlag)
	if err != nil {
		log.Error().Err(err).Msg("Invalid --simulcast")
		return
	}

	// No room, so there are no eyeson stun and turn servers to fall back on.
	iceSettings, err := buildICESettings(nil)
	if err != nil {
		log.Error().Err(err).Msg("Invalid ice configuration")
		return
	}
	if len(iceSettings.Servers) == 0 && !iceSettings.Lite {
		log.Info().Msg("No meeting is joined in a dry run, so the stun and turn " +
			"servers of the eyeson api are not available. Senders on the same " +
			"network do not need them; pass --ice-servers to use your own.")
	}

	monitor := &dryRunMonitor{}
	defer monitor.Stop()

	whipServer := NewWHIPServer(WHIPConfig{
		ListenAddr:     whipListenAddrFlag,
		Path:           whipPathFlag,
		BearerToken:    bearerTokenFlag,
		TLSCertFile:    tlsCertFlag,
		TLSKeyFile:     tlsKeyFlag,
		ICE:            iceSettings,
		VideoCodecs:    codecs,
		PLIInterval:    time.Duration(pliIntervalFlag) * time.Millisecond,
		Simulcast:      simulcastMode,
		SimulcastRID:   simulcastRIDFlag,
		Connect:        monitor.Connect,
		OnSessionState: monitor.SessionState,
		OnSessionEnded: monitor.SessionEnded,
	})

	if err := whipServer.Start(); err != nil {
		log.Error().Err(err).Msg("Failed to start whip-server")
		return
	}

	log.Info().Msgf("Dry run: point a sender at one of the urls above. "+
		"No meeting is joined and the media is discarded. Accepted video codecs: %s",
		videoCodecsFlag)
	log.Info().Msg("Waiting for an incoming connection, press ctrl-c to stop")

	chStop := make(chan os.Signal, 1)
	signal.Notify(chStop, syscall.SIGINT, syscall.SIGTERM)
	<-chStop

	log.Info().Msg("Shutting down")
}

// dryRunMonitor stands in for the meeting connector during a dry run. It
// hands the forwarding path a pair of sinks instead of eyeson tracks and turns
// what happens to a session into the two lines that matter here: a sender
// reached this server, and a sender is gone again.
type dryRunMonitor struct {
	mu        sync.Mutex
	video     *countingSink
	audio     *countingSink
	connected bool
	since     time.Time
	// session counts the sessions that have ended. The reporter carries the
	// number it started with and stops as soon as it changes, which is what
	// ends it - pion delivers its state changes from a goroutine each, so a
	// reporter cannot rely on being told directly.
	session uint64
}

// Connect is the ConnectFunc the WHIP server calls once a session has settled
// on a codec. There is nothing to connect to here, so it hands back two sinks
// that count what they are given.
func (m *dryRunMonitor) Connect(codec VideoCodec) (ghost.RTPWriter, ghost.RTPWriter, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.video = &countingSink{}
	m.audio = &countingSink{}

	log.Info().Msgf("Sender accepted, negotiated video codec: %s", codec.Name)

	return m.video, m.audio, nil
}

// SessionState reports the state of the ingest peer connection. Only the step
// to connected is acted on - that is the moment the sender has actually reached
// this server, rather than merely talked to its http endpoint. Everything that
// ends a session arrives as SessionEnded.
func (m *dryRunMonitor) SessionState(id string, state webrtc.PeerConnectionState) {
	if state != webrtc.PeerConnectionStateConnected {
		return
	}

	m.mu.Lock()
	if m.connected {
		// ice can recover from disconnected, which comes back through here
		m.mu.Unlock()
		return
	}
	m.connected = true
	m.since = time.Now()
	session := m.session
	m.mu.Unlock()

	log.Info().Msgf("Success: a sender has reached this server, session %s is connected", id)

	go m.report(session)
}

// SessionEnded is called whenever an ingest session goes away, connected or
// not, and puts the endpoint back into waiting.
func (m *dryRunMonitor) SessionEnded() {
	ended := m.reset()

	switch {
	case !ended.Started:
		// The WHIP server also reports the end when its http server stops,
		// and there is no session behind that one.
		return
	case ended.Connected:
		log.Info().Msgf("The sender disconnected after %s, received %s",
			time.Since(ended.Since).Round(time.Second), ended.Counts.Describe())
	default:
		log.Warn().Msg("The session ended before it was connected: the sender " +
			"reached the http endpoint, but no media path came up between us")
	}

	log.Info().Msg("Waiting for an incoming connection, press ctrl-c to stop")
}

// Stop ends the reporter on shutdown.
func (m *dryRunMonitor) Stop() {
	m.reset()
}

// dryRunSession is what one session did while it lasted.
type dryRunSession struct {
	// Started is set when there was a session at all.
	Started bool
	// Connected is set when the sender actually reached this server, rather
	// than only its http endpoint.
	Connected bool
	Since     time.Time
	Counts    mediaCounts
}

// reset clears the current session and returns what it did while it lasted.
func (m *dryRunMonitor) reset() dryRunSession {
	counts := m.Counts()

	m.mu.Lock()
	ended := dryRunSession{
		Started:   m.connected || m.video != nil,
		Connected: m.connected,
		Since:     m.since,
		Counts:    counts,
	}
	m.session++
	m.connected = false
	m.video = nil
	m.audio = nil
	m.mu.Unlock()

	return ended
}

// report prints what is arriving while a sender is connected, until the session
// ends. Connected without media means the sender is holding the connection open
// but sending nothing, which is worth saying out loud.
func (m *dryRunMonitor) report(session uint64) {
	ticker := time.NewTicker(dryRunReportInterval)
	defer ticker.Stop()

	previous := uint64(0)
	last := time.Now()

	for now := range ticker.C {
		m.mu.Lock()
		current := m.session
		m.mu.Unlock()
		if current != session {
			return
		}

		counts := m.Counts()
		total := counts.Bytes()

		if total == 0 {
			log.Warn().Msg("Connected, but no media has arrived yet")
			continue
		}

		seconds := now.Sub(last).Seconds()
		last = now
		kbits := float64(total-previous) * 8 / 1000
		previous = total

		log.Info().Msgf("Receiving %s, %.0f kbit/s", counts.Describe(), kbits/seconds)
	}
}

// Counts reads the counters of the current session. They are gone once the
// session has ended, which reads as zero.
func (m *dryRunMonitor) Counts() mediaCounts {
	m.mu.Lock()
	video, audio := m.video, m.audio
	m.mu.Unlock()

	counts := mediaCounts{}
	if video != nil {
		counts.VideoPackets, counts.VideoBytes = video.Read()
	}
	if audio != nil {
		counts.AudioPackets, counts.AudioBytes = audio.Read()
	}

	return counts
}

// countingSink takes the place of an eyeson track during a dry run. The
// forwarding path writes to it exactly as it writes to a meeting, so the whole
// path from the sender to the last step is exercised, and the packets are
// counted instead of being sent anywhere.
type countingSink struct {
	packets atomic.Uint64
	bytes   atomic.Uint64
}

// WriteRTP implements ghost.RTPWriter.
func (c *countingSink) WriteRTP(packet *rtp.Packet) error {
	c.packets.Add(1)
	c.bytes.Add(uint64(packet.MarshalSize()))
	return nil
}

func (c *countingSink) Read() (uint64, uint64) {
	return c.packets.Load(), c.bytes.Load()
}

// mediaCounts is what one session has received so far.
type mediaCounts struct {
	VideoPackets uint64
	VideoBytes   uint64
	AudioPackets uint64
	AudioBytes   uint64
}

// Bytes is everything received, of both kinds.
func (c mediaCounts) Bytes() uint64 {
	return c.VideoBytes + c.AudioBytes
}

// Describe renders the counters for a log line, leaving out a kind that never
// arrived - an audio only or video only sender is a normal thing here.
func (c mediaCounts) Describe() string {
	parts := []string{}
	if c.VideoPackets > 0 {
		parts = append(parts, fmt.Sprintf("video %d packet(s) (%s)",
			c.VideoPackets, formatBytes(c.VideoBytes)))
	}
	if c.AudioPackets > 0 {
		parts = append(parts, fmt.Sprintf("audio %d packet(s) (%s)",
			c.AudioPackets, formatBytes(c.AudioBytes)))
	}
	if len(parts) == 0 {
		return "no media"
	}
	return strings.Join(parts, ", ")
}

func formatBytes(count uint64) string {
	switch {
	case count >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(count)/(1<<30))
	case count >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(count)/(1<<20))
	case count >= 1<<10:
		return fmt.Sprintf("%.1f kB", float64(count)/(1<<10))
	default:
		return fmt.Sprintf("%d B", count)
	}
}