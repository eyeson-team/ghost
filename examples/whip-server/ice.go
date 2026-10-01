package main

import (
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/pion/webrtc/v3"
)

const ICEServersNone = "none"

type ICEServer struct {
	URL      string
	Username string
	Password string
}

type ICESettings struct {
	Servers []ICEServer

	// Replaces the host candidate address, e.g. behind 1:1 NAT.
	PublicIP string

	// Zero means any port.
	UDPPortMin uint16
	UDPPortMax uint16

	Advertise bool

	// Requires this server to be directly reachable.
	Lite bool
}

// Credentials go inline:
//
//	stun:stun.example.com:3478
//	turn:user:pass@turn.example.com:3478?transport=udp
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

func (i ICESettings) SettingEngine() (webrtc.SettingEngine, error) {
	engine := webrtc.SettingEngine{}

	if i.Lite {
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

// RFC 9725 section 4.4.
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
