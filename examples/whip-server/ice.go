package main

import (
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/pion/webrtc/v3"
)

// ICEServersNone disables ice servers entirely, for a server that is directly
// reachable and does not want to wait for anything.
const ICEServersNone = "none"

// ICEServer is one stun or turn server, with the credentials it needs.
type ICEServer struct {
	URL      string
	Username string
	Password string
}

// ICESettings describes how the ingest peer connection reaches the WHIP sender.
//
// Two separate things live in here. The servers are used by this server to
// gather its own candidates, which is what usually decides whether a connection
// works at all: the sender dials us, so our candidates have to be reachable.
// PublicIP and the UDP port range cover the two deployment cases where that
// goes wrong on its own - a cloud VM behind 1:1 NAT, and a firewall that only
// opens a fixed port range.
type ICESettings struct {
	Servers []ICEServer

	// PublicIP replaces the address of host candidates. Needed on cloud VMs
	// where the machine only sees its private address.
	PublicIP string

	// UDPPortMin and UDPPortMax restrict ICE to a fixed port range, so a
	// firewall rule can be written for it. Zero means any port.
	UDPPortMin uint16
	UDPPortMax uint16

	// Advertise sends the servers to the sender via WHIP Link headers.
	Advertise bool

	// Lite runs the ingest agent as an ICE-lite agent: it gathers host
	// candidates only, sends no connectivity checks of its own, and answers
	// the ones the sender sends. That is how most media servers behave, and
	// some senders only start sending once the server has declared itself
	// lite. It requires this server to be directly reachable by the sender.
	Lite bool
}

// ParseICEServers reads a comma separated list of ice server urls. Credentials
// are given inline, which keeps this to one flag instead of four:
//
//	stun:stun.example.com:3478
//	turn:user:pass@turn.example.com:3478?transport=udp
//	turns:user:pass@turn.example.com:5349
func ParseICEServers(list string) ([]ICEServer, error) {
	servers := []ICEServer{}

	for _, entry := range strings.Split(list, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}

		scheme, remainder, found := strings.Cut(entry, ":")
		if !found {
			return nil, fmt.Errorf("ice server %q has no scheme", entry)
		}
		switch strings.ToLower(scheme) {
		case "stun", "stuns", "turn", "turns":
		default:
			return nil, fmt.Errorf("ice server %q must start with stun:, stuns:, turn: or turns:", entry)
		}

		server := ICEServer{}
		// split at the last @: a password may contain one, a host may not
		if at := strings.LastIndex(remainder, "@"); at >= 0 {
			username, password, _ := strings.Cut(remainder[:at], ":")
			server.Username = username
			server.Password = password
			remainder = remainder[at+1:]
		}
		server.URL = scheme + ":" + remainder

		if remainder == "" {
			return nil, fmt.Errorf("ice server %q has no host", entry)
		}

		servers = append(servers, server)
	}

	return servers, nil
}

// ParseUDPPortRange reads a "min-max" port range.
func ParseUDPPortRange(value string) (uint16, uint16, error) {
	if strings.TrimSpace(value) == "" {
		return 0, 0, nil
	}
	first, second, found := strings.Cut(value, "-")
	if !found {
		return 0, 0, fmt.Errorf("expected a range like 50000-50100, got %q", value)
	}
	min, err := strconv.ParseUint(strings.TrimSpace(first), 10, 16)
	if err != nil {
		return 0, 0, fmt.Errorf("invalid lower port in %q: %w", value, err)
	}
	max, err := strconv.ParseUint(strings.TrimSpace(second), 10, 16)
	if err != nil {
		return 0, 0, fmt.Errorf("invalid upper port in %q: %w", value, err)
	}
	if min == 0 || max < min {
		return 0, 0, fmt.Errorf("invalid port range %q", value)
	}
	return uint16(min), uint16(max), nil
}

// ICEServers builds the pion ice server list.
func (i ICESettings) ICEServers() []webrtc.ICEServer {
	servers := make([]webrtc.ICEServer, 0, len(i.Servers))
	for _, server := range i.Servers {
		servers = append(servers, webrtc.ICEServer{
			URLs:       []string{server.URL},
			Username:   server.Username,
			Credential: server.Password,
		})
	}
	return servers
}

// Validate checks the settings that can only fail much later otherwise - a
// public ip that is not an ip is not noticed until the first sender publishes
// and the answer cannot be built.
func (i ICESettings) Validate() error {
	if i.PublicIP == "" {
		return nil
	}
	if net.ParseIP(i.PublicIP) == nil {
		return fmt.Errorf("--public-ip expects an ip address like 1.2.3.4, got %q. "+
			"it is written into the ice candidates, so a hostname or url cannot be used", i.PublicIP)
	}
	return nil
}

// SettingEngine applies the NAT, port range and ice mode settings.
func (i ICESettings) SettingEngine() (webrtc.SettingEngine, error) {
	engine := webrtc.SettingEngine{}

	if i.Lite {
		// A lite agent keeps its host candidates and waits to be pinged, so
		// stun and turn have nothing to contribute here.
		engine.SetLite(true)
	}

	if i.PublicIP != "" {
		engine.SetNAT1To1IPs([]string{i.PublicIP}, webrtc.ICECandidateTypeHost)
	}

	if i.UDPPortMin > 0 && i.UDPPortMax >= i.UDPPortMin {
		if err := engine.SetEphemeralUDPPortRange(i.UDPPortMin, i.UDPPortMax); err != nil {
			return engine, err
		}
	}

	return engine, nil
}

// LinkHeaders renders the ice servers as WHIP Link header values, as described
// in RFC 9725 section 4.4. Senders that read them can use the same servers for
// their own candidate gathering.
func (i ICESettings) LinkHeaders() []string {
	if !i.Advertise {
		return nil
	}

	headers := make([]string, 0, len(i.Servers))
	for _, server := range i.Servers {
		header := fmt.Sprintf(`<%s>; rel="ice-server"`, server.URL)
		if server.Username != "" {
			header += fmt.Sprintf(`; username="%s"; credential="%s"; credential-type="password"`,
				escapeLinkValue(server.Username), escapeLinkValue(server.Password))
		}
		headers = append(headers, header)
	}

	return headers
}

func escapeLinkValue(value string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(value)
}