package main

import (
	"fmt"
	"net"
)

// LocalAddress is one address of one up interface, as used in the startup log.
type LocalAddress struct {
	Interface string
	IP        string
}

// LocalAddresses returns the addresses of the interfaces that are up, IPv4
// first. Loopback and link local addresses are left out: 127.0.0.1 is listed
// separately and a link local address is of no use to a sender.
func LocalAddresses() []LocalAddress {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil
	}

	v4 := []LocalAddress{}
	v6 := []LocalAddress{}

	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addresses, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, address := range addresses {
			network, ok := address.(*net.IPNet)
			if !ok {
				continue
			}
			ip := network.IP
			if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
				continue
			}
			entry := LocalAddress{Interface: iface.Name, IP: ip.String()}
			if ip.To4() != nil {
				v4 = append(v4, entry)
				continue
			}
			v6 = append(v6, LocalAddress{Interface: iface.Name, IP: ip.String()})
		}
	}

	return append(v4, v6...)
}

// EndpointURLs renders the urls a WHIP sender can be pointed at. When the
// server binds to every interface - the default ":8100" - the listen address
// alone tells nobody on the network where to publish to, so every local
// address is spelled out instead.
func EndpointURLs(scheme, listenAddr, path string) []string {
	host, port, err := net.SplitHostPort(listenAddr)
	if err != nil {
		return []string{fmt.Sprintf("%s://%s%s", scheme, listenAddr, path)}
	}

	switch host {
	case "", "0.0.0.0", "::", "[::]":
	default:
		// bound to one address, there is nothing to enumerate
		return []string{fmt.Sprintf("%s://%s%s", scheme, net.JoinHostPort(host, port), path)}
	}

	urls := []string{fmt.Sprintf("%s://%s%s", scheme,
		net.JoinHostPort("127.0.0.1", port), path)}
	for _, address := range LocalAddresses() {
		urls = append(urls, fmt.Sprintf("%s://%s%s", scheme,
			net.JoinHostPort(address.IP, port), path))
	}

	return urls
}