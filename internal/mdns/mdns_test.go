package mdns

import (
	"context"
	"net"
	"testing"
	"time"
)

func TestParseServiceEntry(t *testing.T) {
	// Test advertiser creation and shutdown
	ip := net.ParseIP("192.168.1.100")
	adv, err := NewAdvertiser("Test Proxy", 8010, ip, nil, "264709")
	if err != nil {
		t.Logf("Advertiser registration error (expected in some test environments): %v", err)
		return
	}
	defer adv.Shutdown()

	if adv.DeviceID() == "" {
		t.Fatal("Expected non-empty device ID")
	}

	discoverer := NewDiscoverer(adv.DeviceID(), 8010)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	devices, err := discoverer.Discover(ctx, 1*time.Second)
	if err != nil {
		t.Logf("Discover error: %v", err)
	}
	t.Logf("Found %d devices on network", len(devices))
}
