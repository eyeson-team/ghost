package main

// Check mode: probe the RTSP source only. Nothing in this file talks to
// eyeson; it answers "can the server be reached, what does it describe and
// what does it actually deliver?" and whether that stream is usable by the
// forwarding mode with the current flags.

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/bluenviron/gortsplib/v4"
	"github.com/bluenviron/gortsplib/v4/pkg/base"
	"github.com/bluenviron/gortsplib/v4/pkg/description"
	"github.com/bluenviron/gortsplib/v4/pkg/format"
	"github.com/bluenviron/gortsplib/v4/pkg/format/rtph264"
	"github.com/bluenviron/gortsplib/v4/pkg/format/rtph265"
	"github.com/bluenviron/gortsplib/v4/pkg/liberrors"
	rtsph264 "github.com/bluenviron/mediacommon/pkg/codecs/h264"
	rtsph265 "github.com/bluenviron/mediacommon/pkg/codecs/h265"
	"github.com/pion/rtp"
	log "github.com/rs/zerolog/log"
)

// ---------------------------------------------------------------------------
// report output (stdout, independent of the log level so --quiet still works)
// ---------------------------------------------------------------------------

type checkReport struct {
	failures     int
	warnings     int
	firstFailure string
	summary      [][2]string // ordered key/value lines for the final summary
}

func (r *checkReport) sum(key, f string, a ...interface{}) {
	r.summary = append(r.summary, [2]string{key, fmt.Sprintf(f, a...)})
}

// print the final state and summary block
func (r *checkReport) finish(took time.Duration) {
	r.section("Result")
	state := "OK"
	switch {
	case r.failures > 0:
		state = "FAILED"
	case r.warnings > 0:
		state = fmt.Sprintf("OK (%d warning(s), see above)", r.warnings)
	}
	lines := append([][2]string{{"state", state}}, r.summary...)
	if r.firstFailure != "" {
		lines = append(lines, [2]string{"reason", r.firstFailure})
	}
	lines = append(lines, [2]string{"took", took.Round(10 * time.Millisecond).String()})
	for _, l := range lines {
		fmt.Printf("%-8s %s\n", l[0]+":", l[1])
	}
}

func (r *checkReport) section(title string) { fmt.Printf("\n== %s\n", title) }
func (r *checkReport) info(f string, a ...interface{}) {
	if f == "" {
		fmt.Println()
		return
	}
	fmt.Printf("       %s\n", fmt.Sprintf(f, a...))
}
func (r *checkReport) ok(f string, a ...interface{}) {
	fmt.Printf("[ OK ] %s\n", fmt.Sprintf(f, a...))
}
func (r *checkReport) warn(f string, a ...interface{}) {
	r.warnings++
	fmt.Printf("[WARN] %s\n", fmt.Sprintf(f, a...))
}
func (r *checkReport) fail(f string, a ...interface{}) {
	msg := fmt.Sprintf(f, a...)
	r.failures++
	if r.firstFailure == "" {
		r.firstFailure = msg
	}
	fmt.Printf("[FAIL] %s\n", msg)
}

func ms(d time.Duration) string { return fmt.Sprintf("%d ms", d.Milliseconds()) }

const (
	// The check ends on its own as soon as every track delivered data and
	// the video track has a frame the forwarder can start with. It fails if
	// that does not happen within checkTimeout.
	checkTimeout = 15 * time.Second
	// Minimum amount of video received before finishing, so the measured
	// frame rate and bitrate are meaningful.
	checkMinSample = 1 * time.Second
)

// ---------------------------------------------------------------------------
// per-track statistics collected while playing
// ---------------------------------------------------------------------------

type trackStats struct {
	media *description.Media
	forma format.Format

	packets  uint64
	bytes    uint64
	firstPkt time.Time
	lastPkt  time.Time

	// frame accounting based on RTP timestamps (one timestamp == one frame)
	frames       uint64
	haveTS       bool
	lastTS       uint32
	extTS        int64  // unwrapped timestamp relative to the first packet
	minTS, maxTS int64  // range covered, in clock-rate units
	nonMonotonic uint64 // timestamps going backwards -> usually B-frames

	// video only
	h264Dec        *rtph264.Decoder
	h265Dec        *rtph265.Decoder
	decodeErrors   uint64
	keyframes      uint64
	keyInFrame     bool // current frame already counted as keyframe
	craSeen        bool // H265 open-GOP random access points (not IDR)
	firstKeyframe  time.Time
	lastKeyframe   time.Time
	keyIntervalSum time.Duration
	seiSeen        bool
	inbandVPS      bool
	inbandSPS      bool
	inbandPPS      bool
	inbandSPSInfo  string
	inbandRes      string
}

func (s *trackStats) onPacket(pkt *rtp.Packet, now time.Time) {
	s.packets++
	s.bytes += uint64(len(pkt.Payload))
	if s.firstPkt.IsZero() {
		s.firstPkt = now
	}
	s.lastPkt = now

	if !s.haveTS {
		s.haveTS = true
		s.lastTS = pkt.Timestamp
		s.frames = 1
	} else if pkt.Timestamp != s.lastTS {
		// int32 cast handles the 32-bit wrap-around
		d := int32(pkt.Timestamp - s.lastTS)
		if d < 0 {
			s.nonMonotonic++
		}
		s.extTS += int64(d)
		if s.extTS < s.minTS {
			s.minTS = s.extTS
		}
		if s.extTS > s.maxTS {
			s.maxTS = s.extTS
		}
		s.lastTS = pkt.Timestamp
		s.frames++
		s.keyInFrame = false
	}

	switch {
	case s.h264Dec != nil:
		nalus, err := s.h264Dec.Decode(pkt)
		if err != nil {
			if err != rtph264.ErrMorePacketsNeeded && err != rtph264.ErrNonStartingPacketAndNoPrevious {
				s.decodeErrors++
			}
			return
		}
		s.inspectH264(nalus, now)
	case s.h265Dec != nil:
		nalus, err := s.h265Dec.Decode(pkt)
		if err != nil {
			if err != rtph265.ErrMorePacketsNeeded && err != rtph265.ErrNonStartingPacketAndNoPrevious {
				s.decodeErrors++
			}
			return
		}
		s.inspectH265(nalus, now)
	}
}

func (s *trackStats) markKeyframe(now time.Time) {
	if s.keyInFrame {
		return
	}
	s.keyInFrame = true
	s.keyframes++
	if s.firstKeyframe.IsZero() {
		s.firstKeyframe = now
	} else {
		s.keyIntervalSum += now.Sub(s.lastKeyframe)
	}
	s.lastKeyframe = now
}

func (s *trackStats) inspectH264(nalus [][]byte, now time.Time) {
	for _, nalu := range nalus {
		if len(nalu) == 0 {
			continue
		}
		switch rtsph264.NALUType(nalu[0] & 0x1F) {
		case rtsph264.NALUTypeIDR:
			s.markKeyframe(now)
		case rtsph264.NALUTypeSEI:
			s.seiSeen = true
		case rtsph264.NALUTypeSPS:
			s.inbandSPS = true
			if s.inbandSPSInfo == "" {
				s.inbandSPSInfo = describeH264SPS(nalu)
				s.inbandRes = spsResolution(false, nalu)
			}
		case rtsph264.NALUTypePPS:
			s.inbandPPS = true
		}
	}
}

func (s *trackStats) inspectH265(nalus [][]byte, now time.Time) {
	for _, nalu := range nalus {
		if len(nalu) == 0 {
			continue
		}
		switch rtsph265.NALUType((nalu[0] >> 1) & 0x3F) {
		case rtsph265.NALUType_IDR_W_RADL, rtsph265.NALUType_IDR_N_LP:
			s.markKeyframe(now)
		case rtsph265.NALUType_CRA_NUT:
			s.craSeen = true
		case rtsph265.NALUType_PREFIX_SEI_NUT:
			s.seiSeen = true
		case rtsph265.NALUType_VPS_NUT:
			s.inbandVPS = true
		case rtsph265.NALUType_SPS_NUT:
			s.inbandSPS = true
			if s.inbandSPSInfo == "" {
				s.inbandSPSInfo = describeH265SPS(nalu)
				s.inbandRes = spsResolution(true, nalu)
			}
		case rtsph265.NALUType_PPS_NUT:
			s.inbandPPS = true
		}
	}
}

// ---------------------------------------------------------------------------
// SPS helpers
// ---------------------------------------------------------------------------

func describeH264SPS(buf []byte) string {
	if len(buf) == 0 {
		return ""
	}
	var sps rtsph264.SPS
	if err := sps.Unmarshal(buf); err != nil {
		return fmt.Sprintf("unparsable SPS (%v)", err)
	}
	profile := map[uint8]string{
		66: "Baseline", 77: "Main", 88: "Extended", 100: "High",
		110: "High 10", 122: "High 4:2:2", 244: "High 4:4:4",
	}[sps.ProfileIdc]
	if profile == "" {
		profile = fmt.Sprintf("profile_idc %d", sps.ProfileIdc)
	}
	if sps.ProfileIdc == 66 && sps.ConstraintSet1Flag {
		profile = "Constrained Baseline"
	}
	out := fmt.Sprintf("%dx%d, %s, level %.1f", sps.Width(), sps.Height(),
		profile, float64(sps.LevelIdc)/10)
	if fps := sps.FPS(); fps > 0 {
		out += fmt.Sprintf(", %.2f fps (signalled)", fps)
	}
	return out
}

func describeH265SPS(buf []byte) string {
	if len(buf) == 0 {
		return ""
	}
	var sps rtsph265.SPS
	if err := sps.Unmarshal(buf); err != nil {
		return fmt.Sprintf("unparsable SPS (%v)", err)
	}
	ptl := sps.ProfileTierLevel
	profile := map[uint8]string{1: "Main", 2: "Main 10", 3: "Main Still Picture", 4: "Range Extensions"}[ptl.GeneralProfileIdc]
	if profile == "" {
		profile = fmt.Sprintf("profile_idc %d", ptl.GeneralProfileIdc)
	}
	out := fmt.Sprintf("%dx%d, %s, level %.1f", sps.Width(), sps.Height(),
		profile, float64(ptl.GeneralLevelIdc)/30)
	if fps := sps.FPS(); fps > 0 {
		out += fmt.Sprintf(", %.2f fps (signalled)", fps)
	}
	return out
}

// spsResolution returns "WxH" or "" if the SPS is missing or unparsable.
func spsResolution(codecH265 bool, buf []byte) string {
	if len(buf) == 0 {
		return ""
	}
	if codecH265 {
		var sps rtsph265.SPS
		if sps.Unmarshal(buf) == nil {
			return fmt.Sprintf("%dx%d", sps.Width(), sps.Height())
		}
		return ""
	}
	var sps rtsph264.SPS
	if sps.Unmarshal(buf) == nil {
		return fmt.Sprintf("%dx%d", sps.Width(), sps.Height())
	}
	return ""
}

// ---------------------------------------------------------------------------
// error hints
// ---------------------------------------------------------------------------

func explainRTSPError(err error) string {
	var bad liberrors.ErrClientBadStatusCode
	if errors.As(err, &bad) {
		switch bad.Code {
		case base.StatusUnauthorized:
			return "authentication required or credentials rejected " +
				"(pass them in the URL: rtsp://user:pass@host/path)"
		case base.StatusNotFound:
			return "stream path not found on the server, check the URL path"
		case base.StatusForbidden:
			return "access forbidden by the server"
		case base.StatusUnsupportedTransport:
			return "server rejected the offered transport (UDP/TCP)"
		}
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "timeout waiting for the server"
	}
	return ""
}

func withHint(err error) string {
	if hint := explainRTSPError(err); hint != "" {
		return fmt.Sprintf("%v -> %s", err, hint)
	}
	return err.Error()
}

// ---------------------------------------------------------------------------
// the check itself
// ---------------------------------------------------------------------------

// runRtspCheck probes rtspConnectURL and prints a report to stdout.
// It returns false if the stream is not usable for forwarding.
func runRtspCheck(rtspConnectURL string) bool {
	r := &checkReport{}

	checkStart := time.Now()
	defer func() { r.finish(time.Since(checkStart)) }()

	// --- 1. URL ------------------------------------------------------------
	r.section("URL")
	u, err := base.ParseURL(rtspConnectURL)
	if err != nil {
		r.fail("invalid RTSP URL: %v", err)
		return false
	}
	r.info("url:       %s", (*url.URL)(u).Redacted())
	r.sum("url", "%s", (*url.URL)(u).Redacted())

	host := u.Host
	if u.Port() == "" {
		port := "554"
		if u.Scheme == "rtsps" {
			port = "322"
		}
		host = net.JoinHostPort(u.Hostname(), port)
	}

	// --- 2. plain TCP reachability ----------------------------------------
	// Done separately so "host/port unreachable" is clearly distinguishable
	// from RTSP-level problems.
	r.section("Reachability")
	t0 := time.Now()
	conn, err := net.DialTimeout("tcp", host, 5*time.Second)
	if err != nil {
		r.sum("source", "unreachable")
		r.fail("TCP connect to %s failed: %v", host, err)
		return false
	}
	r.ok("TCP connect to %s (%s) in %s", host, conn.RemoteAddr(), ms(time.Since(t0)))
	r.sum("source", "reachable")
	conn.Close()

	// --- 3. RTSP session ---------------------------------------------------
	// Same client settings as the forward mode (defaults), so the result is
	// representative.
	var (
		packetsLost     uint64
		decodeErrs      uint64
		transportSwitch atomic.Value
	)
	c := gortsplib.Client{
		OnRequest: func(req *base.Request) {
			log.Debug().Msgf("RTSP > %s %s", req.Method, req.URL.CloneWithoutCredentials())
		},
		OnResponse: func(res *base.Response) {
			log.Debug().Msgf("RTSP < %d %s", res.StatusCode, res.StatusMessage)
		},
		OnTransportSwitch: func(err error) { transportSwitch.Store(err.Error()) },
		OnPacketsLost:     func(lost uint64) { atomic.AddUint64(&packetsLost, lost) },
		OnDecodeError:     func(err error) { atomic.AddUint64(&decodeErrs, 1) },
	}
	if err := c.Start(u.Scheme, u.Host); err != nil {
		r.fail("RTSP client start failed: %v", err)
		return false
	}
	defer c.Close()

	r.section("RTSP handshake")
	t0 = time.Now()
	optRes, err := c.Options(u)
	if err != nil {
		// Some cameras answer OPTIONS badly but stream fine, so only warn.
		r.warn("OPTIONS failed: %s", withHint(err))
	} else {
		r.ok("OPTIONS answered in %s", ms(time.Since(t0)))
		if v, ok := optRes.Header["Server"]; ok && len(v) > 0 {
			r.info("server:  %s", v[0])
			r.summary[len(r.summary)-1][1] += ", server " + v[0]
		}
		if v, ok := optRes.Header["Public"]; ok && len(v) > 0 {
			r.info("methods: %s", strings.Join(v, ", "))
		}
	}

	t0 = time.Now()
	session, _, err := c.Describe(u)
	if err != nil {
		r.fail("DESCRIBE failed: %s", withHint(err))
		return false
	}
	r.ok("DESCRIBE answered in %s", ms(time.Since(t0)))

	// --- 4. what the server describes (SDP) --------------------------------
	r.section("Described tracks (SDP)")
	if session.Title != "" {
		r.info("title: %s", session.Title)
	}
	if len(session.Medias) == 0 {
		r.fail("server describes no tracks at all")
		return false
	}

	var fh264 *format.H264
	mediaH264 := session.FindFormat(&fh264)
	var fh265 *format.H265
	mediaH265 := session.FindFormat(&fh265)

	// same selection as the forward mode
	codecH265, found := selectVideoTrack(session)
	wantCodec := codecName(codecH265)
	var selMedia *description.Media
	var selFormat format.Format
	if found && codecH265 {
		selMedia, selFormat = mediaH265, fh265
	} else if found {
		selMedia, selFormat = mediaH264, fh264
	}

	for i, m := range session.Medias {
		label := fmt.Sprintf("track %d: %s", i, m.Type)
		if m.Control != "" {
			label += fmt.Sprintf(" (control=%s)", m.Control)
		}
		if m.IsBackChannel {
			label += " [back channel]"
		}
		if m == selMedia {
			label += "  <- would be forwarded"
		}
		r.info("%s", label)
		for _, f := range m.Formats {
			r.info("  - %s, payload type %d, clock %d Hz", f.Codec(), f.PayloadType(), f.ClockRate())
			if fmtp := f.FMTP(); len(fmtp) > 0 {
				keys := make([]string, 0, len(fmtp))
				for k := range fmtp {
					keys = append(keys, k)
				}
				sort.Strings(keys)
				parts := make([]string, 0, len(keys))
				for _, k := range keys {
					parts = append(parts, k+"="+fmtp[k])
				}
				r.info("    fmtp: %s", strings.Join(parts, "; "))
			}
			switch vf := f.(type) {
			case *format.H264:
				if sps, _ := vf.SafeParams(); sps != nil {
					r.info("    %s", describeH264SPS(sps))
				}
			case *format.H265:
				if _, sps, _ := vf.SafeParams(); sps != nil {
					r.info("    %s", describeH265SPS(sps))
				}
			}
		}
	}

	if selMedia == nil {
		r.fail("no H264 or H265 video track found (source offers: %s)", offeredCodecs(session))
		return false
	}
	r.ok("%s video track will be forwarded", wantCodec)
	if mediaH264 != nil && mediaH265 != nil {
		r.info("source offers H264 and H265, H264 is preferred")
	}

	// parameter sets in the SDP: the H264 forwarder prepends them to keyframes
	var sdpSPS, sdpPPS, sdpVPS []byte
	if codecH265 {
		sdpVPS, sdpSPS, sdpPPS = fh265.SafeParams()
	} else {
		sdpSPS, sdpPPS = fh264.SafeParams()
	}

	// --- 5. SETUP + PLAY and measure --------------------------------------
	r.section("Stream data")

	var mu sync.Mutex
	stats := map[*description.Media]*trackStats{}

	for _, m := range session.Medias {
		if m.IsBackChannel {
			continue
		}
		if _, err := c.Setup(session.BaseURL, m, 0, 0); err != nil {
			if m == selMedia {
				r.fail("SETUP of the %s track failed: %s", wantCodec, withHint(err))
				return false
			}
			r.warn("SETUP of %s track failed (not needed for forwarding): %s", m.Type, withHint(err))
			continue
		}
		ts := &trackStats{media: m, forma: m.Formats[0]}
		if m == selMedia {
			ts.forma = selFormat
			if codecH265 {
				if dec, err := fh265.CreateDecoder(); err == nil {
					ts.h265Dec = dec
				}
			} else {
				if dec, err := fh264.CreateDecoder(); err == nil {
					ts.h264Dec = dec
				}
			}
		}
		stats[m] = ts
	}

	c.OnPacketRTPAny(func(m *description.Media, f format.Format, pkt *rtp.Packet) {
		now := time.Now()
		mu.Lock()
		defer mu.Unlock()
		if ts, ok := stats[m]; ok {
			if m == selMedia && f != selFormat {
				return // other format multiplexed on the same media
			}
			ts.onPacket(pkt, now)
		}
	})

	// Install the handler before PLAY, so Ctrl-C always ends up in the
	// summary instead of killing the process.
	chStop := make(chan os.Signal, 1)
	signal.Notify(chStop, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(chStop)

	playStart := time.Now()
	if _, err := c.Play(nil); err != nil {
		r.fail("PLAY failed: %s", withHint(err))
		return false
	}
	r.ok("PLAY accepted in %s", ms(time.Since(playStart)))
	r.info("receiving stream data (at most %s) ...", checkTimeout)

	// ready reports whether we have seen enough: data on every track, a frame
	// the forwarder can start with, and a short sample for the rates.
	// Must be called with mu held.
	ready := func() bool {
		for _, ts := range stats {
			if ts.packets == 0 {
				return false
			}
		}
		sel := stats[selMedia]
		startable := sel.keyframes > 0 || passThroughFlag || (!codecH265 && sel.seiSeen)
		return startable && time.Since(sel.firstPkt) >= checkMinSample
	}

	waitErr := make(chan error, 1)
	go func() { waitErr <- c.Wait() }()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	timeout := time.After(checkTimeout)

	var streamErr error
	var timedOut, aborted bool
wait:
	for {
		select {
		case <-ticker.C:
			mu.Lock()
			done := ready()
			mu.Unlock()
			if done {
				break wait
			}
		case <-timeout:
			timedOut = true
			break wait
		case <-chStop:
			aborted = true
			break wait
		case streamErr = <-waitErr:
			break wait
		}
	}
	elapsed := time.Since(playStart)
	clientStats := c.Stats()
	c.Close()

	r.info("received for %s", elapsed.Round(time.Millisecond))
	if timedOut {
		r.info("gave up waiting after %s", checkTimeout)
	}
	if aborted {
		r.fail("check aborted by user before it was complete")
	}
	if streamErr != nil {
		r.fail("stream ended after %s: %s", elapsed.Round(time.Millisecond), withHint(streamErr))
	}
	if v := transportSwitch.Load(); v != nil {
		r.info("transport: %s", v)
	}

	mu.Lock()
	defer mu.Unlock()

	for i, m := range session.Medias {
		ts, ok := stats[m]
		if !ok {
			continue
		}
		r.info("")
		r.info("track %d: %s / %s", i, m.Type, ts.forma.Codec())
		key := string(m.Type)
		if ts.packets == 0 {
			if m == selMedia {
				r.fail("no %s video data received", wantCodec)
			} else {
				r.warn("no RTP packets received")
			}
			r.sum(key, "%s, no data", ts.forma.Codec())
			continue
		}

		active := ts.lastPkt.Sub(ts.firstPkt)
		kbps := 0.0
		if active > 0 {
			kbps = float64(ts.bytes*8) / active.Seconds() / 1000
		}
		r.info("first packet after %s", ms(ts.firstPkt.Sub(playStart)))
		line := fmt.Sprintf("%d packets, %.1f KiB payload", ts.packets, float64(ts.bytes)/1024)
		if active > 0 {
			line += fmt.Sprintf(", ~%.0f kbit/s", float64(ts.bytes*8)/active.Seconds()/1000)
		}
		r.info("%s", line)
		if mst, ok := clientStats.Session.Medias[m]; ok {
			if fs, ok := mst.Formats[ts.forma]; ok {
				r.info("lost %d packets, jitter %.1f", fs.RTPPacketsLost, fs.RTPPacketsJitter)
			}
		}

		if m.Type != description.MediaTypeVideo {
			r.sum(key, "%s, ~%.0f kbit/s (not forwarded)", ts.forma.Codec(), kbps)
			continue
		}

		fps := 0.0
		if span := ts.maxTS - ts.minTS; ts.frames > 1 && span > 0 {
			fps = float64(ts.frames-1) / (float64(span) / float64(ts.forma.ClockRate()))
			r.info("%d frames, ~%.1f fps measured", ts.frames, fps)
		}

		if m != selMedia {
			r.sum(key, "%s, ~%.0f kbit/s (not forwarded)", ts.forma.Codec(), kbps)
			continue
		}

		res := spsResolution(codecH265, sdpSPS)
		if res == "" {
			res = ts.inbandRes
		}
		if res == "" {
			res = "unknown size"
		}
		r.sum(key, "%s %s, ~%.0f fps, ~%.0f kbit/s", ts.forma.Codec(), res, fps, kbps)

		// Everything below mirrors what the forward mode depends on.
		if ts.decodeErrors > 0 {
			r.warn("%d %s depacketization errors", ts.decodeErrors, wantCodec)
		}
		if ts.nonMonotonic > 0 {
			r.warn("%d non-monotonic timestamps: B-frames detected, which the forwarder does not support "+
				"(disable B-frames on the source)", ts.nonMonotonic)
		}

		switch {
		case ts.keyframes > 0:
			line := fmt.Sprintf("%d keyframe(s), first after %s", ts.keyframes, ms(ts.firstKeyframe.Sub(playStart)))
			if ts.keyframes > 1 {
				line += fmt.Sprintf(", every ~%.1f s", (ts.keyIntervalSum / time.Duration(ts.keyframes-1)).Seconds())
			}
			r.ok("%s", line)
			if ts.firstKeyframe.Sub(playStart) > 5*time.Second {
				r.warn("first keyframe took long: the meeting stays black until it arrives " +
					"(consider a shorter keyframe interval on the source)")
			}
		case !codecH265 && ts.seiSeen:
			r.warn("no IDR keyframe seen, but SEI (recovery point) present: the forwarder starts on SEI")
		case passThroughFlag:
			r.warn("no keyframe received within %s (--passthrough forwards anyway, "+
				"but the picture may be broken until one arrives)", elapsed.Round(time.Second))
		case codecH265 && ts.craSeen:
			r.fail("no IDR keyframe within %s, only CRA frames (open GOP): the forwarder only starts on IDR "+
				"frames, so configure the source for closed GOP / IDR keyframes (or use --passthrough)",
				elapsed.Round(time.Second))
		default:
			r.fail("no keyframe received within %s: the forwarder drops everything until the first "+
				"keyframe, shorten the keyframe interval on the source (or use --passthrough)",
				elapsed.Round(time.Second))
		}

		// parameter sets
		if ts.inbandSPSInfo != "" && sdpSPS == nil {
			r.info("in-band SPS: %s", ts.inbandSPSInfo)
		}
		missing := []string{}
		if sdpSPS == nil {
			missing = append(missing, "SPS")
		}
		if sdpPPS == nil {
			missing = append(missing, "PPS")
		}
		if codecH265 && sdpVPS == nil {
			missing = append(missing, "VPS")
		}
		inband := ts.inbandSPS && ts.inbandPPS && (!codecH265 || ts.inbandVPS)
		switch {
		case len(missing) == 0:
			if inband {
				r.ok("parameter sets present in SDP and repeated in-band")
			} else {
				r.ok("parameter sets present in SDP")
			}
		case inband:
			r.warn("%s missing in SDP, but sent in-band by the stream", strings.Join(missing, "/"))
		default:
			r.warn("%s missing in SDP and not seen in-band: decoders in the meeting may not be able "+
				"to start the stream", strings.Join(missing, "/"))
		}
	}

	if n := atomic.LoadUint64(&packetsLost); n > 0 {
		r.warn("%d RTP packets reported lost during the check", n)
	}
	if n := atomic.LoadUint64(&decodeErrs); n > 0 {
		r.warn("%d non-fatal RTP decode errors", n)
	}

	return r.failures == 0
}