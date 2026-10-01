package main

import (
	"net/http"
	"strings"
	"time"

	"github.com/pion/webrtc/v3"
	log "github.com/rs/zerolog/log"
)

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recorder := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()

		next.ServeHTTP(recorder, r)

		log.Debug().Msgf("%s %s from %s -> %d (%s, content-type %q, user-agent %q)",
			r.Method, r.URL.Path, r.RemoteAddr, recorder.status,
			time.Since(start).Round(time.Millisecond),
			r.Header.Get("Content-Type"), r.Header.Get("User-Agent"))
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status  int
	written bool
}

func (s *statusRecorder) WriteHeader(status int) {
	if !s.written {
		s.status = status
		s.written = true
	}
	s.ResponseWriter.WriteHeader(status)
}

func (s *statusRecorder) Write(data []byte) (int, error) {
	s.written = true
	return s.ResponseWriter.Write(data)
}

type OfferICE struct {
	Candidates      int
	Lite            bool
	Trickle         bool
	EndOfCandidates bool
}

func InspectOfferICE(offer string) OfferICE {
	summary := OfferICE{}

	for _, line := range strings.Split(strings.ReplaceAll(offer, "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "a=candidate:"):
			summary.Candidates++
		case line == "a=ice-lite":
			summary.Lite = true
		case strings.HasPrefix(line, "a=ice-options:") &&
			strings.Contains(line, "trickle"):
			summary.Trickle = true
		case line == "a=end-of-candidates":
			summary.EndOfCandidates = true
		}
	}

	return summary
}

// An ice-lite sender without candidates deadlocks a full agent. Answering lite
// lets us learn its address as a peer reflexive candidate.
func (o OfferICE) NeedsLiteAnswer() bool {
	return o.Lite && o.Candidates == 0
}

func LogOfferICE(offer string) OfferICE {
	summary := InspectOfferICE(offer)

	log.Debug().Msgf("Offer ice: %d candidate(s), ice-lite=%t, trickle=%t, end-of-candidates=%t",
		summary.Candidates, summary.Lite, summary.Trickle, summary.EndOfCandidates)

	switch {
	case summary.Candidates > 0:
	case summary.NeedsLiteAnswer():
		log.Info().Msg("The offer announces ice-lite and carries no candidates: " +
			"answering as a lite agent, so the sender drives the connection")
	default:
		log.Info().Msg("The offer carries no ice candidates: expecting the sender " +
			"to trickle them in with PATCH, or to be learned as a peer reflexive " +
			"candidate from its STUN checks")
	}

	return summary
}

func LogSessionICE(id string, pc *webrtc.PeerConnection) {
	pc.OnICEGatheringStateChange(func(state webrtc.ICEGathererState) {
		log.Debug().Msgf("WHIP session %s ice gathering state: %s", id, state)
	})

	pc.OnICEConnectionStateChange(func(state webrtc.ICEConnectionState) {
		log.Debug().Msgf("WHIP session %s ice connection state: %s", id, state)
		if state == webrtc.ICEConnectionStateConnected {
			LogSelectedCandidatePair(id, pc)
		}
	})
}

func LogSelectedCandidatePair(id string, pc *webrtc.PeerConnection) {
	sctp := pc.SCTP()
	if sctp == nil {
		return
	}
	dtls := sctp.Transport()
	if dtls == nil {
		return
	}
	ice := dtls.ICETransport()
	if ice == nil {
		return
	}

	pair, err := ice.GetSelectedCandidatePair()
	if err != nil || pair == nil {
		return
	}

	log.Info().Msgf("WHIP session %s selected candidate pair: local %s <- remote %s",
		id, pair.Local.String(), pair.Remote.String())
}