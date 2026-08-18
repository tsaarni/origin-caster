package netutil

import (
	"errors"
	"fmt"
	"net"
)

// DetectIPForTarget discovers the local IPv4 address that routes to targetIP.
// It queries the OS routing table by creating an unbound UDP socket (no network packets sent).
func DetectIPForTarget(targetIP net.IP) (net.IP, error) {
	if targetIP == nil {
		return nil, errors.New("target IP cannot be nil")
	}

	conn, err := net.Dial("udp4", net.JoinHostPort(targetIP.String(), "8009"))
	if err != nil {
		return nil, fmt.Errorf("failed to determine route to target %s: %w", targetIP, err)
	}
	defer conn.Close()

	localAddr, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok || localAddr.IP == nil {
		return nil, fmt.Errorf("could not determine local IP for route to %s", targetIP)
	}

	ip4 := localAddr.IP.To4()
	if ip4 == nil {
		return nil, fmt.Errorf("resolved local address is not IPv4 for route to %s", targetIP)
	}

	return ip4, nil
}
