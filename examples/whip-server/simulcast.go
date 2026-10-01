package main

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/webrtc/v3"
	log "github.com/rs/zerolog/log"
)

const simulcastProbeWindow = 1500 * time.Millisecond

const SimulcastAuto = "auto"

type simulcastLayer struct {
	rid   string
	track *webrtc.TrackRemote
	bytes atomic.Uint64
}

// One layer is forwarded per session, no switching. The others must still be
// read, or their receivers stall.
type simulcastSelector struct {
	forced string

	mu      sync.Mutex
	layers  []*simulcastLayer
	started bool

	chosen atomic.Pointer[simulcastLayer]
}

func newSimulcastSelector(rid string) *simulcastSelector {
	if strings.EqualFold(rid, SimulcastAuto) {
		rid = ""
	}
	return &simulcastSelector{forced: rid}
}

func (s *simulcastSelector) add(track *webrtc.TrackRemote) *simulcastLayer {
	layer := &simulcastLayer{rid: track.RID(), track: track}

	s.mu.Lock()
	s.layers = append(s.layers, layer)
	first := !s.started
	s.started = true
	s.mu.Unlock()

	if first {
		log.Info().Msgf("Sender publishes simulcast, measuring the layers for %s",
			simulcastProbeWindow)
		time.AfterFunc(simulcastProbeWindow, s.pick)
	}

	if s.forced != "" && layer.rid == s.forced {
		s.choose(layer, "requested with --simulcast-rid")
	}

	return layer
}

func (s *simulcastSelector) pick() {
	if s.chosen.Load() != nil {
		return
	}

	s.mu.Lock()
	layers := append([]*simulcastLayer(nil), s.layers...)
	s.mu.Unlock()

	if len(layers) == 0 {
		return
	}

	sort.SliceStable(layers, func(i, j int) bool {
		return layers[i].bytes.Load() > layers[j].bytes.Load()
	})

	seconds := simulcastProbeWindow.Seconds()
	described := make([]string, 0, len(layers))
	for _, layer := range layers {
		described = append(described, fmt.Sprintf("rid %q %.0f kbit/s",
			layer.rid, float64(layer.bytes.Load())*8/1000/seconds))
	}
	log.Info().Msgf("Simulcast layers: %s", strings.Join(described, ", "))

	reason := "highest bitrate"
	if s.forced != "" {
		log.Warn().Msgf("The sender publishes no layer with rid %q, "+
			"falling back to the one with the highest bitrate", s.forced)
	}
	s.choose(layers[0], reason)
}

func (s *simulcastSelector) choose(layer *simulcastLayer, reason string) {
	if !s.chosen.CompareAndSwap(nil, layer) {
		return
	}
	log.Info().Msgf("Forwarding simulcast layer rid %q (%s), dropping the others",
		layer.rid, reason)
}

func (s *simulcastSelector) isChosen(layer *simulcastLayer) bool {
	return s.chosen.Load() == layer
}

// Decline is the default: only one stream is forwarded anyway.
const (
	SimulcastSelect  = "select"
	SimulcastDecline = "decline"
)

func ParseSimulcastMode(mode string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case SimulcastSelect:
		return SimulcastSelect, nil
	case SimulcastDecline:
		return SimulcastDecline, nil
	default:
		return "", fmt.Errorf("unknown simulcast mode %q, known are %s and %s",
			mode, SimulcastSelect, SimulcastDecline)
	}
}

// Strips rid and simulcast lines (RFC 8853 section 5.3) and the rid extensions.
// mid stays, bundle needs it.
func DeclineSimulcast(offer string) (string, bool) {
	lineEnd := "\n"
	if strings.Contains(offer, "\r\n") {
		lineEnd = "\r\n"
	}

	lines := strings.Split(strings.ReplaceAll(offer, "\r\n", "\n"), "\n")
	kept := make([]string, 0, len(lines))
	found := false

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, "a=simulcast:"):
			found = true
			continue
		case strings.HasPrefix(trimmed, "a=rid:"):
			continue
		case strings.HasPrefix(trimmed, "a=extmap:") &&
			(strings.Contains(trimmed, "sdes:rtp-stream-id") ||
				strings.Contains(trimmed, "sdes:repaired-rtp-stream-id")):
			continue
		}
		kept = append(kept, line)
	}

	if !found {
		return offer, false
	}
	return strings.Join(kept, lineEnd), true
}
