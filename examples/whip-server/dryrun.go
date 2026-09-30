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

const dryRunReportInterval = 5 * time.Second

// runDryRun serves the WHIP endpoint without joining a meeting, to test a
// sender setup. Media is counted and dropped.
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

type dryRunMonitor struct {
	mu        sync.Mutex
	video     *countingSink
	audio     *countingSink
	connected bool
	since     time.Time
	// Incremented per ended session, which stops the running reporter.
	session uint64
}

func (m *dryRunMonitor) Connect(codec VideoCodec) (ghost.RTPWriter, ghost.RTPWriter, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.video = &countingSink{}
	m.audio = &countingSink{}

	log.Info().Msgf("Sender accepted, negotiated video codec: %s", codec.Name)

	return m.video, m.audio, nil
}

func (m *dryRunMonitor) SessionState(id string, state webrtc.PeerConnectionState) {
	if state != webrtc.PeerConnectionStateConnected {
		return
	}

	m.mu.Lock()
	if m.connected {
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

func (m *dryRunMonitor) SessionEnded() {
	ended := m.reset()

	switch {
	case !ended.Started:
		// Also reported on http shutdown, with no session behind it.
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

func (m *dryRunMonitor) Stop() {
	m.reset()
}

type dryRunSession struct {
	Started   bool
	Connected bool
	Since     time.Time
	Counts    mediaCounts
}

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

type countingSink struct {
	packets atomic.Uint64
	bytes   atomic.Uint64
}

func (c *countingSink) WriteRTP(packet *rtp.Packet) error {
	c.packets.Add(1)
	c.bytes.Add(uint64(packet.MarshalSize()))
	return nil
}

func (c *countingSink) Read() (uint64, uint64) {
	return c.packets.Load(), c.bytes.Load()
}

type mediaCounts struct {
	VideoPackets uint64
	VideoBytes   uint64
	AudioPackets uint64
	AudioBytes   uint64
}

func (c mediaCounts) Bytes() uint64 {
	return c.VideoBytes + c.AudioBytes
}

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
