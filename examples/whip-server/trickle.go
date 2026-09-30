package main

import (
	"io"
	"net/http"
	"strings"

	"github.com/pion/webrtc/v3"
	log "github.com/rs/zerolog/log"
)

// RFC 8840.
const trickleContentType = "application/trickle-ice-sdpfrag"

// Needed for senders whose offer carries no candidates.
func (s *WHIPServer) handleTrickle(w http.ResponseWriter, r *http.Request, resourceID string) {
	if !s.authorized(r) {
		w.Header().Set("WWW-Authenticate", "Bearer")
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	s.mu.Lock()
	sess := s.session
	s.mu.Unlock()

	if sess == nil || sess.id != resourceID || sess.closed {
		http.Error(w, "unknown resource", http.StatusNotFound)
		return
	}

	// application/sdp would be an ice restart, which is not supported.
	if contentType := r.Header.Get("Content-Type"); !strings.HasPrefix(contentType, trickleContentType) {
		log.Warn().Msgf("PATCH with content-type %q, only %s is supported",
			contentType, trickleContentType)
		http.Error(w, "expected content-type "+trickleContentType,
			http.StatusUnsupportedMediaType)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 64*1024))
	if err != nil {
		http.Error(w, "unreadable body", http.StatusBadRequest)
		return
	}

	logSDP("ICE fragment from the WHIP sender", string(body))

	candidates, endOfCandidates := ParseICEFragment(string(body))

	added := 0
	for _, candidate := range candidates {
		if err := sess.pc.AddICECandidate(candidate); err != nil {
			log.Warn().Err(err).Msgf("Failed to add remote candidate %q", candidate.Candidate)
			continue
		}
		log.Debug().Msgf("Added remote candidate %q", candidate.Candidate)
		added++
	}

	if endOfCandidates {
		log.Debug().Msg("Sender signalled end-of-candidates")
	}

	log.Info().Msgf("WHIP session %s: added %d of %d trickled ice candidate(s)",
		sess.id, added, len(candidates))

	w.WriteHeader(http.StatusNoContent)
}

func ParseICEFragment(fragment string) ([]webrtc.ICECandidateInit, bool) {
	candidates := []webrtc.ICECandidateInit{}
	endOfCandidates := false

	ufrag := ""
	mid := ""
	index := -1

	for _, line := range strings.Split(strings.ReplaceAll(fragment, "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(line)

		switch {
		case strings.HasPrefix(line, "m="):
			index++
			mid = ""
		case strings.HasPrefix(line, "a=mid:"):
			mid = strings.TrimSpace(strings.TrimPrefix(line, "a=mid:"))
		case strings.HasPrefix(line, "a=ice-ufrag:"):
			ufrag = strings.TrimSpace(strings.TrimPrefix(line, "a=ice-ufrag:"))
		case line == "a=end-of-candidates" || line == "end-of-candidates":
			endOfCandidates = true
		case strings.HasPrefix(line, "a=candidate:") || strings.HasPrefix(line, "candidate:"):
			candidate := webrtc.ICECandidateInit{
				Candidate: strings.TrimPrefix(line, "a="),
			}
			if mid != "" {
				value := mid
				candidate.SDPMid = &value
			}
			if index >= 0 {
				value := uint16(index)
				candidate.SDPMLineIndex = &value
			}
			if ufrag != "" {
				value := ufrag
				candidate.UsernameFragment = &value
			}
			candidates = append(candidates, candidate)
		}
	}

	return candidates, endOfCandidates
}
