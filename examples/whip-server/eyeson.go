package main

import (
	"fmt"
	"sync"
	"time"

	"github.com/eyeson-team/eyeson-go"
	ghost "github.com/eyeson-team/ghost/v2"
	log "github.com/rs/zerolog/log"
)

const meetingConnectTimeout = 15 * time.Second

// ghost fixes the codec when the client is created, so the connection is made
// lazily and rebuilt when the codec changes.
type MeetingConnector struct {
	room          *eyeson.UserService
	clientOptions []ghost.ClientOption
	noAudio       bool
	// Not called for connections we tear down ourselves.
	OnTerminated func()

	mu      sync.Mutex
	current *meetingConn
	closed  bool
}

type meetingConn struct {
	codec   VideoCodec
	client  ghost.EyesonClient
	video   ghost.RTPWriter
	audio   ghost.RTPWriter
	dropped bool // set when this connection was replaced or closed by us
}

func NewMeetingConnector(room *eyeson.UserService, clientOptions []ghost.ClientOption,
	noAudio bool) *MeetingConnector {
	return &MeetingConnector{
		room:          room,
		clientOptions: clientOptions,
		noAudio:       noAudio,
	}
}

// A different codec replaces the connection: the participant leaves and rejoins.
func (m *MeetingConnector) Connect(codec VideoCodec) (ghost.RTPWriter, ghost.RTPWriter, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.closed {
		return nil, nil, fmt.Errorf("connector is closed")
	}

	if m.current != nil {
		if m.current.codec.Name == codec.Name {
			log.Debug().Msgf("Reusing eyeson connection with codec %s", codec.Name)
			return m.current.video, m.current.audio, nil
		}
		log.Info().Msgf("Codec changed from %s to %s, reconnecting to the meeting",
			m.current.codec.Name, codec.Name)
		m.dropCurrentLocked()
	}

	conn, err := m.dial(codec)
	if err != nil {
		return nil, nil, err
	}
	m.current = conn

	return conn.video, conn.audio, nil
}

func (m *MeetingConnector) dial(codec VideoCodec) (*meetingConn, error) {
	options := make([]ghost.ClientOption, 0, len(m.clientOptions)+1)
	options = append(options, m.clientOptions...)
	options = append(options, codec.GhostOption)

	client, err := ghost.NewClient(m.room.Data, options...)
	if err != nil {
		return nil, fmt.Errorf("failed to create eyeson client: %w", err)
	}

	conn := &meetingConn{codec: codec, client: client}

	type tracks struct {
		video ghost.RTPWriter
		audio ghost.RTPWriter
	}
	connected := make(chan tracks, 1)

	client.SetConnectedHandler(func(ok bool, video ghost.RTPWriter, audio ghost.RTPWriter) {
		select {
		case connected <- tracks{video: video, audio: audio}:
		default:
		}
	})

	client.SetTerminatedHandler(func() {
		m.mu.Lock()
		dropped := conn.dropped
		m.mu.Unlock()
		if dropped {
			return
		}
		log.Info().Msg("Call terminated")
		if m.OnTerminated != nil {
			m.OnTerminated()
		}
	})

	if traceFlag {
		client.SetDataChannelHandler(func(data []byte) {
			log.Trace().Msgf("DC message: %s", string(data))
		})
	}

	log.Info().Msgf("Connecting to the meeting with video codec %s", codec.Name)

	if err := client.Call(); err != nil {
		client.Destroy()
		return nil, fmt.Errorf("failed to call: %w", err)
	}

	select {
	case t := <-connected:
		conn.video = t.video
		conn.audio = t.audio
		if m.noAudio {
			conn.audio = nil
		}
		log.Info().Msgf("Meeting connection is up, video codec %s", codec.Name)
		return conn, nil
	case <-time.After(meetingConnectTimeout):
		conn.dropped = true
		client.Destroy()
		return nil, fmt.Errorf("timed out after %s waiting for the meeting connection",
			meetingConnectTimeout)
	}
}

// Caller holds the lock.
func (m *MeetingConnector) dropCurrentLocked() {
	if m.current == nil {
		return
	}
	m.current.dropped = true
	m.current.client.Destroy()
	m.current = nil
}

func (m *MeetingConnector) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.closed = true
	if m.current == nil {
		return
	}
	m.current.dropped = true
	if err := m.current.client.TerminateCall(); err != nil {
		log.Debug().Err(err).Msg("Failed to terminate call")
	}
	m.current.client.Destroy()
	m.current = nil
}
