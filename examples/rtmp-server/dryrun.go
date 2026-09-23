package main

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/bluenviron/mediacommon/pkg/codecs/h264"
	"github.com/notedit/rtmp/av"
	"github.com/notedit/rtmp/codec/aac"
	rtmph264 "github.com/notedit/rtmp/codec/h264"
	"github.com/notedit/rtmp/format/rtmp"
	log "github.com/rs/zerolog/log"
)

const (
	dryRunStatsInterval = 2 * time.Second
	dryRunStallTimeout  = 5 * time.Second
)

// dryRunStats holds the counters of one rtmp connection. It is written by the
// reading goroutine and read by the status ticker, hence the mutex.
type dryRunStats struct {
	mu sync.Mutex

	connectedAt  time.Time
	firstPacket  time.Time
	lastPacket   time.Time
	lastStreamTS time.Duration

	videoPackets int
	keyFrames    int
	audioPackets int
	otherPackets int
	nalus        int
	decodeErrors int
	bytes        int64

	gotVideoConfig bool

	// values at the last status print, used for per-interval rates
	lastPrintAt    time.Time
	lastPrintBytes int64
	lastPrintVideo int
}

// runDryRun starts an rtmp server without connecting to an eyeson meeting.
// It reports every incoming connection and what is received on it, and keeps
// accepting new connections until it is stopped with ctrl-c.
func runDryRun(listenAddr string) {
	u, err := url.Parse(listenAddr)
	if err != nil {
		log.Error().Err(err).Msgf("Invalid rtmp listen address %q", listenAddr)
		return
	}
	hostPort := rtmp.UrlGetHost(u)

	lis, err := net.Listen("tcp", hostPort)
	if err != nil {
		log.Error().Err(err).Msg("Failed to start RTMP server")
		return
	}

	log.Info().Msg("DRY-RUN: not connecting to any meeting")
	printListenAddresses(lis.Addr().(*net.TCPAddr), u.Path)

	rtmpServer := rtmp.NewServer()

	var (
		activeMu sync.Mutex
		activeNC net.Conn
		stopping bool
	)

	rtmpServer.LogEvent = func(c *rtmp.Conn, nc net.Conn, e int) {
		if e == rtmp.EventHandshakeFailed {
			log.Warn().Str("address", nc.RemoteAddr().String()).
				Msg("RTMP handshake failed (client did not start publishing)")
		}
	}
	rtmpServer.HandleConn = func(c *rtmp.Conn, nc net.Conn) {
		handleDryRunConn(c, nc)
	}

	// ctrl-c: stop accepting, drop the active client and leave.
	chStop := make(chan os.Signal, 1)
	signal.Notify(chStop, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-chStop
		fmt.Fprintln(os.Stderr)
		log.Info().Msg("Stopping RTMP server")
		activeMu.Lock()
		stopping = true
		if activeNC != nil {
			activeNC.Close()
		}
		activeMu.Unlock()
		lis.Close()
	}()

	for {
		log.Info().Msg("STATE: waiting for incoming RTMP connection ... (ctrl-c to quit)")

		nc, err := lis.Accept()
		if err != nil {
			activeMu.Lock()
			done := stopping
			activeMu.Unlock()
			if done || errors.Is(err, net.ErrClosed) {
				break
			}
			log.Warn().Err(err).Msg("Accept failed")
			time.Sleep(time.Second)
			continue
		}

		activeMu.Lock()
		activeNC = nc
		activeMu.Unlock()

		log.Info().Str("address", nc.RemoteAddr().String()).
			Msg("STATE: client connected (tcp), doing RTMP handshake")
		rtmpServer.HandleNetConn(nc)
		nc.Close()

		activeMu.Lock()
		activeNC = nil
		done := stopping
		activeMu.Unlock()
		if done {
			break
		}
	}
	log.Info().Msg("Bye")
}

func handleDryRunConn(c *rtmp.Conn, nc net.Conn) {
	remote := nc.RemoteAddr().String()
	streamPath := ""
	if c.URL != nil {
		streamPath = c.URL.Path
	}
	log.Info().Str("address", remote).Str("stream", streamPath).
		Msg("STATE: successfully connected, client is publishing")

	stats := &dryRunStats{connectedAt: time.Now()}
	stats.lastPrintAt = stats.connectedAt

	done := make(chan struct{})
	go dryRunStatusTicker(stats, remote, done)

	var reason error
	for {
		packet, err := c.ReadPacket()
		if err != nil {
			reason = err
			break
		}
		processDryRunPacket(stats, remote, packet)
	}
	close(done)

	stats.mu.Lock()
	defer stats.mu.Unlock()
	log.Info().Str("address", remote).
		Str("reason", reason.Error()).
		Str("duration", time.Since(stats.connectedAt).Round(time.Millisecond).String()).
		Int("video_pkts", stats.videoPackets).
		Int("keyframes", stats.keyFrames).
		Int("audio_pkts", stats.audioPackets).
		Str("received", formatBytes(stats.bytes)).
		Int("decode_errors", stats.decodeErrors).
		Msg("STATE: client disconnected")
}

func processDryRunPacket(stats *dryRunStats, remote string, packet av.Packet) {
	now := time.Now()

	stats.mu.Lock()
	first := stats.firstPacket.IsZero()
	if first {
		stats.firstPacket = now
	}
	wasStalled := !stats.lastPacket.IsZero() && now.Sub(stats.lastPacket) > dryRunStallTimeout
	stats.lastPacket = now
	stats.lastStreamTS = packet.Time
	stats.bytes += int64(len(packet.Data))
	stats.mu.Unlock()

	if first {
		log.Info().Str("address", remote).Msg("STATE: receiving data")
	}
	if wasStalled {
		log.Info().Str("address", remote).Msg("STATE: receiving data again")
	}

	switch packet.Type {
	case av.H264DecoderConfig:
		logH264Config(remote, packet.Data)
		stats.mu.Lock()
		stats.gotVideoConfig = true
		stats.mu.Unlock()

	case av.H264:
		// Same AVCC -> NALU step the real mode performs, to see whether the
		// stream would be usable.
		nalus, err := h264.AVCCUnmarshal(packet.Data)
		stats.mu.Lock()
		stats.videoPackets++
		if packet.IsKeyFrame {
			stats.keyFrames++
		}
		if err != nil {
			stats.decodeErrors++
		} else {
			stats.nalus += len(nalus)
		}
		firstVideoWithoutConfig := stats.videoPackets == 1 && !stats.gotVideoConfig
		firstKeyframe := packet.IsKeyFrame && stats.keyFrames == 1
		stats.mu.Unlock()

		if err != nil {
			log.Warn().Err(err).Msg("Failed to decode H264 packet")
		}
		if firstVideoWithoutConfig {
			log.Warn().Msg("H264 frames arrive before the decoder config (SPS/PPS)")
		}
		if firstKeyframe {
			log.Info().Str("address", remote).Msg("First video keyframe received")
		}

	case av.AACDecoderConfig:
		logAACConfig(remote, packet.Data)

	case av.AAC, av.OPUS:
		stats.mu.Lock()
		stats.audioPackets++
		stats.mu.Unlock()

	case av.Metadata:
		log.Debug().Str("address", remote).Msg("Stream metadata received")

	default:
		stats.mu.Lock()
		stats.otherPackets++
		stats.mu.Unlock()
	}

	log.Trace().Msgf("packet: %s len=%d", packet.String(), len(packet.Data))
}

func dryRunStatusTicker(stats *dryRunStats, remote string, done <-chan struct{}) {
	ticker := time.NewTicker(dryRunStatsInterval)
	defer ticker.Stop()
	stallReported := false

	for {
		select {
		case <-done:
			return
		case now := <-ticker.C:
			stats.mu.Lock()
			elapsed := now.Sub(stats.lastPrintAt).Seconds()
			kbps := float64(stats.bytes-stats.lastPrintBytes) * 8 / 1000 / elapsed
			fps := float64(stats.videoPackets-stats.lastPrintVideo) / elapsed
			stats.lastPrintAt = now
			stats.lastPrintBytes = stats.bytes
			stats.lastPrintVideo = stats.videoPackets

			idle := time.Duration(0)
			if !stats.lastPacket.IsZero() {
				idle = now.Sub(stats.lastPacket)
			} else {
				idle = now.Sub(stats.connectedAt)
			}
			video, keys, audio := stats.videoPackets, stats.keyFrames, stats.audioPackets
			total, streamTS := stats.bytes, stats.lastStreamTS
			stats.mu.Unlock()

			if idle > dryRunStallTimeout {
				if !stallReported {
					log.Warn().Str("address", remote).
						Msgf("STATE: connected, but no data for %s", idle.Round(time.Second))
					stallReported = true
				}
				continue
			}
			stallReported = false

			log.Info().
				Int("video_pkts", video).
				Int("keyframes", keys).
				Int("audio_pkts", audio).
				Str("received", formatBytes(total)).
				Str("bitrate", fmt.Sprintf("%.0f kbit/s", kbps)).
				Str("fps", fmt.Sprintf("%.1f", fps)).
				Str("stream_time", streamTS.Round(100*time.Millisecond).String()).
				Msg("STATE: receiving")
		}
	}
}

func logH264Config(remote string, data []byte) {
	codec, err := rtmph264.FromDecoderConfig(data)
	if err != nil {
		log.Warn().Err(err).Msg("Received invalid H264 decoder config")
		return
	}
	ev := log.Info().Str("address", remote)
	if len(codec.SPS) > 0 {
		var sps h264.SPS
		if err := sps.Unmarshal(codec.SPS[0]); err == nil {
			ev = ev.Str("resolution", fmt.Sprintf("%dx%d", sps.Width(), sps.Height()))
			if fps := sps.FPS(); fps > 0 {
				ev = ev.Str("fps", fmt.Sprintf("%.2f", fps))
			}
			ev = ev.Int("profile", int(sps.ProfileIdc)).Int("h264_level", int(sps.LevelIdc))
		}
	}
	ev.Int("sps", len(codec.SPS)).Int("pps", len(codec.PPS)).
		Msg("H264 decoder config received")
}

func logAACConfig(remote string, data []byte) {
	cfg, err := aac.ParseMPEG4AudioConfigBytes(data)
	if err != nil {
		log.Info().Str("address", remote).Msg("Audio decoder config received")
		return
	}
	log.Info().Str("address", remote).
		Int("sample_rate", cfg.SampleRate).
		Int("channels", cfg.ChannelLayout.Count()).
		Msg("AAC decoder config received (audio is not forwarded to the meeting)")
}

// printListenAddresses prints the rtmp urls clients can publish to. For a
// wildcard listen address every IPv4 address of every active interface is
// listed.
func printListenAddresses(addr *net.TCPAddr, path string) {
	port := addr.Port
	if !addr.IP.IsUnspecified() {
		if ip4 := addr.IP.To4(); ip4 != nil {
			log.Info().Msgf("RTMP server listening on rtmp://%s:%d%s", ip4, port, path)
		} else {
			log.Info().Msgf("RTMP server listening on %s (not an IPv4 address)", addr)
		}
		return
	}

	log.Info().Msgf("RTMP server listening on port %d, reachable via:", port)
	ifaces, err := net.Interfaces()
	if err != nil {
		log.Warn().Err(err).Msg("Could not list network interfaces")
		return
	}
	found := 0
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipNet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip4 := ipNet.IP.To4()
			if ip4 == nil {
				continue // ipv4 only
			}
			log.Info().Msgf("    rtmp://%s:%d%s   (%s)", ip4, port, path, iface.Name)
			found++
		}
	}
	if found == 0 {
		log.Warn().Msg("No IPv4 addresses found on any active interface")
	}
}

func formatBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
}