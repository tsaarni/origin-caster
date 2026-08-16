package netutil

import (
	"errors"
	"fmt"
	"net"
	"strings"
)

// NetworkInfo contains information about the detected local network interface.
type NetworkInfo struct {
	IP        net.IP
	Interface *net.Interface
}

// DetectLANIP discovers the primary routable IPv4 address and interface on the local machine.
// If ifaceName is provided, it specifically searches for that interface.
func DetectLANIP(ifaceName string) (*NetworkInfo, error) {
	if ifaceName != "" {
		iface, err := net.InterfaceByName(ifaceName)
		if err != nil {
			return nil, fmt.Errorf("interface %q not found: %w", ifaceName, err)
		}
		ip, err := getIPv4FromInterface(iface)
		if err != nil {
			return nil, fmt.Errorf("failed to get IPv4 for %q: %w", ifaceName, err)
		}
		return &NetworkInfo{IP: ip, Interface: iface}, nil
	}

	// Strategy 1: Outbound UDP connection probe (does not send packets)
	conn, err := net.Dial("udp4", "8.8.8.8:80")
	if err == nil {
		defer conn.Close()
		localAddr := conn.LocalAddr().(*net.UDPAddr)
		if localAddr.IP != nil && !localAddr.IP.IsLoopback() {
			iface, ifaceErr := getInterfaceByIP(localAddr.IP)
			if ifaceErr == nil {
				return &NetworkInfo{IP: localAddr.IP, Interface: iface}, nil
			}
			return &NetworkInfo{IP: localAddr.IP, Interface: nil}, nil
		}
	}

	// Strategy 2: Enumerate interfaces, prioritizing active Wi-Fi/Ethernet (en0, en1 on macOS)
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil, fmt.Errorf("failed to enumerate network interfaces: %w", err)
	}

	var candidates []*NetworkInfo

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
			info := &NetworkInfo{IP: ip, Interface: &iface}
			// On macOS, en0 is almost always primary Wi-Fi or Ethernet
			if strings.EqualFold(iface.Name, "en0") {
				return info, nil
			}
			candidates = append(candidates, info)
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

func getInterfaceByIP(targetIP net.IP) (*net.Interface, error) {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	for _, iface := range interfaces {
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			var ip net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip != nil && ip.Equal(targetIP) {
				return &iface, nil
			}
		}
	}
	return nil, fmt.Errorf("interface for IP %s not found", targetIP.String())
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
