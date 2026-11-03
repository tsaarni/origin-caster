package mdns

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/grandcat/zeroconf"
)

// DiscoveredDevice holds details about a Chromecast device on the LAN.
type DiscoveredDevice struct {
	ID           string
	FriendlyName string
	ModelName    string
	IP           net.IP
	Port         int
}

// Discoverer scans the LAN for physical Chromecast devices.
type Discoverer struct{}

// NewDiscoverer creates a Chromecast scanner.
func NewDiscoverer() *Discoverer {
	return &Discoverer{}
}

// Discover scans for Cast devices for the specified timeout duration.
func (d *Discoverer) Discover(ctx context.Context, timeout time.Duration) ([]*DiscoveredDevice, error) {
	started := time.Now()
	slog.Debug("Discovery started", "component", "mdns", "service", "_googlecast._tcp", "timeout", timeout)
	resolver, err := zeroconf.NewResolver(nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create zeroconf resolver: %w", err)
	}

	entries := make(chan *zeroconf.ServiceEntry)
	var devices []*DiscoveredDevice
	var mu sync.Mutex
	done := make(chan struct{})
	accepted, rejected, duplicates := 0, 0, 0

	subCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	go func() {
		defer close(done)
		for entry := range entries {
			dev := parseServiceEntry(entry)
			if dev == nil {
				rejected++
				slog.Debug("Discovery entry skipped", "instance", entry.Instance, "reason", "no usable address")
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
				accepted++
				slog.Debug("Chromecast discovered", "device_id", dev.ID, "name", dev.FriendlyName, "model", dev.ModelName, "address", net.JoinHostPort(dev.IP.String(), fmt.Sprint(dev.Port)))
			} else {
				duplicates++
				slog.Debug("Duplicate discovery entry skipped", "device_id", dev.ID)
			}
			mu.Unlock()
		}
	}()

	err = resolver.Browse(subCtx, "_googlecast._tcp", "local.", entries)
	if err != nil {
		return nil, fmt.Errorf("failed to browse mDNS: %w", err)
	}

	<-subCtx.Done()
	<-done
	slog.Debug("Discovery completed", "component", "mdns", "duration", time.Since(started), "devices", accepted, "rejected", rejected, "duplicates", duplicates, "reason", subCtx.Err())
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	mu.Lock()
	defer mu.Unlock()
	return devices, nil
}

// FindTarget returns the device matching targetName, or the first device
// discovered when targetName is empty.
func (d *Discoverer) FindTarget(ctx context.Context, targetName string, timeout time.Duration) (*DiscoveredDevice, error) {
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
				slog.Debug("Discovery target selected", "name", dev.FriendlyName, "target", targetName)
				return dev, nil
			}
		}
		return nil, fmt.Errorf("no device matching %q found among %d discovered devices", targetName, len(devices))
	}

	slog.Debug("Discovery target selected", "name", devices[0].FriendlyName, "reason", "first discovered device")
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
		ips, err := net.LookupIP(entry.HostName)
		if err != nil {
			slog.Debug("Discovery hostname lookup failed", "host", entry.HostName, "error", err)
		} else {
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
		IP:   ip,
		Port: entry.Port,
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
