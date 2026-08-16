package netutil

import (
	"errors"
	"fmt"
	"net"
	"strings"
)

// DetectLANIP discovers the primary routable IPv4 address on the local machine.
func DetectLANIP() (net.IP, error) {
	// Strategy 1: Outbound UDP connection probe (does not send packets)
	conn, err := net.Dial("udp4", "8.8.8.8:80")
	if err == nil {
		defer conn.Close()
		localAddr := conn.LocalAddr().(*net.UDPAddr)
		if localAddr.IP != nil && !localAddr.IP.IsLoopback() {
			return localAddr.IP, nil
		}
	}

	// Strategy 2: Enumerate interfaces, prioritizing active Wi-Fi/Ethernet (en0, en1 on macOS)
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil, fmt.Errorf("failed to enumerate network interfaces: %w", err)
	}

	var candidates []net.IP

	for _, iface := range interfaces {
		// Filter out down or loopback interfaces
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}

		ip, err := getIPv4FromInterface(&iface)
		if err != nil || ip == nil {
			continue
		}

		// Prefer private address ranges (RFC 1918)
		if isPrivateIPv4(ip) {
			// On macOS, en0 is almost always primary Wi-Fi or Ethernet
			if strings.EqualFold(iface.Name, "en0") {
				return ip, nil
			}
			candidates = append(candidates, ip)
		}
	}

	if len(candidates) > 0 {
		return candidates[0], nil
	}

	return nil, errors.New("no active LAN IPv4 interface found")
}

func getIPv4FromInterface(iface *net.Interface) (net.IP, error) {
	addrs, err := iface.Addrs()
	if err != nil {
		return nil, err
	}
	for _, addr := range addrs {
		var ip net.IP
		switch v := addr.(type) {
		case *net.IPNet:
			ip = v.IP
		case *net.IPAddr:
			ip = v.IP
		}
		if ip != nil && ip.To4() != nil && !ip.IsLoopback() {
			return ip.To4(), nil
		}
	}
	return nil, fmt.Errorf("no IPv4 address on interface %s", iface.Name)
}

func isPrivateIPv4(ip net.IP) bool {
	ip4 := ip.To4()
	if ip4 == nil {
		return false
	}
	// 10.0.0.0/8
	if ip4[0] == 10 {
		return true
	}
	// 172.16.0.0/12
	if ip4[0] == 172 && ip4[1] >= 16 && ip4[1] <= 31 {
		return true
	}
	// 192.168.0.0/16
	if ip4[0] == 192 && ip4[1] == 168 {
		return true
	}
	// 169.254.0.0/16 (Link local)
	if ip4[0] == 169 && ip4[1] == 254 {
		return true
	}
	return false
}
