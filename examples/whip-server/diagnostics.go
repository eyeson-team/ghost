package main

import (
	"net/http"
	"strings"
	"time"

	"github.com/pion/webrtc/v3"
	log "github.com/rs/zerolog/log"
)

// logRequests writes one line per http request. Without it there is no way to
// tell a sender that never talked to us from one whose PATCH or DELETE was
// turned down - both look like silence in the log.
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

// OfferICE is what the ice part of an offer says: how many candidates the
// sender put in it, and how it intends to negotiate.
type OfferICE struct {
	// Candidates counts the a=candidate lines of the offer.
	Candidates int
	// Lite is set when the sender announced a=ice-lite. Such a sender never
	// sends connectivity checks itself, so this server has to check against
	// its candidates - and it cannot, when there are none.
	Lite bool
	// Trickle is set when the sender announced a=ice-options:trickle.
	Trickle bool
	// EndOfCandidates is set when the offer says its candidate list is final.
	EndOfCandidates bool
}

// InspectOfferICE summarises the ice part of an offer for the log.
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

// NeedsLiteAnswer reports whether answering this offer as a full ice agent is
// hopeless. A sender that announces ice-lite sends no connectivity checks, so
// this server has to send them - and with no candidates in the offer there is
// nowhere to send them to. Both ends then wait for the other until the session
// times out.
//
// Answering as a lite agent breaks that deadlock: this server stops checking
// and waits to be pinged, which is what such a sender expects of a media
// server, and the address it pings from is learned as a peer reflexive
// candidate. It needs this server to be directly reachable, but so does every
// other outcome available here.
func (o OfferICE) NeedsLiteAnswer() bool {
	return o.Lite && o.Candidates == 0
}

// LogOfferICE says what the offer brings to the ice negotiation, warns when it
// brings nothing to connect to, and returns the summary so the answer can be
// built to match. An offer without candidates is not an error in itself - the
// sender may trickle them in later - but if it never does, the session times
// out half a minute later with nothing in the log that points at the cause.
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
		log.Warn().Msg("The offer carries no ice candidates: the sender has to trickle " +
			"them in with PATCH, otherwise this session cannot connect.")
	}

	return summary
}

// LogSessionICE attaches the ice level logging to an ingest peer connection.
// The connection state alone only says that something failed; the ice state,
// the gathering state and the selected pair say where it got stuck.
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

// LogSelectedCandidatePair reports which pair of addresses the media actually
// travels over - host to host on a lan, or via a turn relay.
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