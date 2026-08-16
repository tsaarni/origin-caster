package mdns

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/grandcat/zeroconf"
)

// DiscoveredDevice holds details about a physical or proxy Chromecast device on the LAN.
type DiscoveredDevice struct {
	ID           string
	FriendlyName string
	ModelName    string
	Capabilities string
	IP           net.IP
	Port         int
	HostName     string
	TXTRecords   []string
}

// Advertiser publishes the virtual Chromecast mDNS service.
type Advertiser struct {
	server   *zeroconf.Server
	deviceID string
	name     string
	port     int
}

// NewAdvertiser starts advertising the virtual Chromecast device on the network.
func NewAdvertiser(friendlyName string, port int, localIP net.IP, iface *net.Interface, capabilities string) (*Advertiser, error) {
	// Generate consistent device ID based on hostname/friendlyName
	hasher := md5.New()
	hasher.Write([]byte(friendlyName + localIP.String()))
	deviceID := hex.EncodeToString(hasher.Sum(nil))

	cdHasher := md5.New()
	cdHasher.Write([]byte("cd" + deviceID))
	cloudDeviceID := strings.ToUpper(hex.EncodeToString(cdHasher.Sum(nil)))

	instanceName := fmt.Sprintf("Chromecast-Proxy-%s", deviceID)

	ca := "264709"
	if capabilities != "" {
		ca = capabilities
	}

	txtRecords := []string{
		"id=" + deviceID,
		"cd=" + cloudDeviceID,
		"rm=",
		"ve=05",
		"md=Chromecast",
		"ic=/setup/icon.png",
		"fn=" + friendlyName,
		"ca=" + ca,
		"st=0",
		"bs=FA8FCA82B682",
		"nf=1",
		"rs=",
	}

	var ifaces []net.Interface
	if iface != nil {
		ifaces = append(ifaces, *iface)
	}

	server, err := zeroconf.Register(
		instanceName,
		"_googlecast._tcp",
		"local.",
		port,
		txtRecords,
		ifaces,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to register mDNS service: %w", err)
	}

	return &Advertiser{
		server:   server,
		deviceID: deviceID,
		name:     friendlyName,
		port:     port,
	}, nil
}

// DeviceID returns the UUID/MD5 hex identifier of the advertised device.
func (a *Advertiser) DeviceID() string {
	return a.deviceID
}

// Shutdown stops the mDNS advertisement.
func (a *Advertiser) Shutdown() {
	if a.server != nil {
		a.server.Shutdown()
	}
}

// Discoverer scans the local network for physical Chromecast devices.
type Discoverer struct {
	selfDeviceID string
	selfPort     int
}

// NewDiscoverer creates a new Chromecast scanner.
func NewDiscoverer(selfDeviceID string, selfPort int) *Discoverer {
	return &Discoverer{
		selfDeviceID: selfDeviceID,
		selfPort:     selfPort,
	}
}

// Discover scans for Cast devices for the specified timeout duration.
func (d *Discoverer) Discover(ctx context.Context, timeout time.Duration) ([]*DiscoveredDevice, error) {
	resolver, err := zeroconf.NewResolver(nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create zeroconf resolver: %w", err)
	}

	entries := make(chan *zeroconf.ServiceEntry)
	var devices []*DiscoveredDevice
	var mu sync.Mutex

	subCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	go func() {
		for entry := range entries {
			dev := parseServiceEntry(entry)
			if dev == nil {
				continue
			}
			// Skip self
			if d.selfDeviceID != "" && dev.ID == d.selfDeviceID {
				continue
			}
			if d.selfPort != 0 && dev.Port == d.selfPort && isLocalIP(dev.IP) {
				continue
			}

			mu.Lock()
			// Avoid duplicate device IDs
			exists := false
			for _, existing := range devices {
				if existing.ID != "" && existing.ID == dev.ID {
					exists = true
					break
				}
				if existing.IP.Equal(dev.IP) && existing.Port == dev.Port {
					exists = true
					break
				}
			}
			if !exists {
				devices = append(devices, dev)
			}
			mu.Unlock()
		}
	}()

	err = resolver.Browse(subCtx, "_googlecast._tcp", "local.", entries)
	if err != nil {
		return nil, fmt.Errorf("failed to browse mDNS: %w", err)
	}

	<-subCtx.Done()

	mu.Lock()
	defer mu.Unlock()
	return devices, nil
}

// FindTarget attempts to find a matching physical Chromecast or returns the first discovered device.
func (d *Discoverer) FindTarget(ctx context.Context, targetIP, targetName string, timeout time.Duration) (*DiscoveredDevice, error) {
	// If explicit IP provided, we can return immediately or probe
	if targetIP != "" {
		ip := net.ParseIP(targetIP)
		if ip == nil {
			return nil, fmt.Errorf("invalid target IP %q", targetIP)
		}
		return &DiscoveredDevice{
			ID:           "manual-" + targetIP,
			FriendlyName: fmt.Sprintf("Target (%s)", targetIP),
			ModelName:    "Chromecast",
			IP:           ip,
			Port:         8009,
		}, nil
	}

	devices, err := d.Discover(ctx, timeout)
	if err != nil {
		return nil, err
	}

	if len(devices) == 0 {
		return nil, fmt.Errorf("no physical Chromecast devices discovered on LAN within %v", timeout)
	}

	if targetName != "" {
		lowerTarget := strings.ToLower(targetName)
		for _, dev := range devices {
			if strings.Contains(strings.ToLower(dev.FriendlyName), lowerTarget) ||
				strings.Contains(strings.ToLower(dev.ModelName), lowerTarget) {
				return dev, nil
			}
		}
		return nil, fmt.Errorf("no device matching %q found among %d discovered devices", targetName, len(devices))
	}

	// Default to first found physical device
	return devices[0], nil
}

func parseServiceEntry(entry *zeroconf.ServiceEntry) *DiscoveredDevice {
	var ip net.IP
	for _, a := range entry.AddrIPv4 {
		if a != nil && a.To4() != nil {
			ip = a.To4()
			break
		}
	}

	if ip == nil && entry.HostName != "" {
		if ips, err := net.LookupIP(entry.HostName); err == nil {
			for _, a := range ips {
				if a != nil && a.To4() != nil {
					ip = a.To4()
					break
				}
			}
		}
	}

	if ip == nil && len(entry.AddrIPv6) > 0 {
		ip = entry.AddrIPv6[0]
	}

	if ip == nil {
		return nil
	}

	dev := &DiscoveredDevice{
		IP:         ip,
		Port:       entry.Port,
		HostName:   entry.HostName,
		TXTRecords: entry.Text,
	}

	for _, txt := range entry.Text {
		parts := strings.SplitN(txt, "=", 2)
		if len(parts) == 2 {
			k, v := parts[0], parts[1]
			switch k {
			case "id":
				dev.ID = v
			case "fn":
				dev.FriendlyName = v
			case "md":
				dev.ModelName = v
			case "ca":
				dev.Capabilities = v
			}
		}
	}

	if dev.FriendlyName == "" {
		dev.FriendlyName = entry.Instance
	}
	if dev.ModelName == "" {
		dev.ModelName = "Chromecast"
	}

	return dev
}

func isLocalIP(ip net.IP) bool {
	if ip == nil {
		return false
	}
	if ip.IsLoopback() {
		return true
	}
	// Check against all local interface IPs
	ifaces, err := net.Interfaces()
	if err != nil {
		return false
	}
	for _, iface := range ifaces {
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			var localIP net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				localIP = v.IP
			case *net.IPAddr:
				localIP = v.IP
			}
			if localIP != nil && localIP.Equal(ip) {
				return true
			}
		}
	}
	return false
}
