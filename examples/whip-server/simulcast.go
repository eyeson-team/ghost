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

// simulcastProbeWindow is how long the layers of a simulcast sender are
// measured before one of them is picked. Long enough for every layer to have
// sent its first frames, short enough not to matter against the time the
// meeting connection takes to come up anyway.
const simulcastProbeWindow = 1500 * time.Millisecond

// SimulcastAuto picks the layer that carries the most data during the probe
// window, which is the one with the highest resolution.
const SimulcastAuto = "auto"

// simulcastLayer is one rid of a simulcast video track.
type simulcastLayer struct {
	rid   string
	track *webrtc.TrackRemote
	bytes atomic.Uint64
}

// simulcastSelector decides which layer of a simulcast sender is forwarded.
//
// The meeting takes a single video stream and this example does not switch
// layers: switching would mean waiting for a keyframe on the new layer and
// rewriting sequence numbers and timestamps so the receiver sees one
// continuous stream. So one layer is picked per session and the others are
// read and dropped - they still have to be read, or their receivers stall.
type simulcastSelector struct {
	// forced is the rid given with --simulcast-rid, empty for auto.
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

// add registers a layer as its track shows up. The probe window starts with
// the first layer. A layer that was asked for by name is taken right away.
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

// pick runs once the probe window is over and takes the busiest layer, unless
// a layer has been chosen already.
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

// isChosen reports whether this layer is the one to forward. It is false for
// every layer until the choice has been made.
func (s *simulcastSelector) isChosen(layer *simulcastLayer) bool {
	return s.chosen.Load() == layer
}