package main

import (
	"fmt"
	"net"
)

type LocalAddress struct {
	Interface string
	IP        string
}

// IPv4 only. IPv6 privacy addresses would clutter the startup log.
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

func EndpointURLs(scheme, listenAddr, path string) []string {
	host, port, err := net.SplitHostPort(listenAddr)
	if err != nil {
		return []string{fmt.Sprintf("%s://%s%s", scheme, listenAddr, path)}
	}

	switch host {
	case "", "0.0.0.0", "::", "[::]":
	default:
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
