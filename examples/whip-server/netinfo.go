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

// LocalAddresses returns the IPv4 addresses of the interfaces that are up.
// Loopback and link local addresses are left out: 127.0.0.1 is listed
// separately and a link local address is of no use to a sender.
//
// IPv6 is left out as well. A machine on a v6 network has several of them -
// the stable one plus a temporary privacy address per rotation - and they push
// the address a sender is actually going to use off the top of the startup log.
// The endpoint still listens on them; they are just not advertised. A flag to
// list them can be added when a sender needs one.
func LocalAddresses() []LocalAddress {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil
	}

	addresses := []LocalAddress{}

	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		found, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, address := range found {
			network, ok := address.(*net.IPNet)
			if !ok {
				continue
			}
			ip := network.IP
			if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
				continue
			}
			if ip.To4() == nil {
				continue
			}
			addresses = append(addresses, LocalAddress{Interface: iface.Name, IP: ip.String()})
		}
	}

	return addresses
}

// EndpointURLs renders the urls a WHIP sender can be pointed at. When the
// server binds to every interface - the default ":8100" - the listen address
// alone tells nobody on the network where to publish to, so every local IPv4
// address is spelled out instead. Binding to one address explicitly is listed
// as it was given, IPv6 included.
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