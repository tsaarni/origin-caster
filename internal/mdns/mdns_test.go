package mdns

import (
	"net"
	"testing"

	"github.com/grandcat/zeroconf"
)

func TestParseServiceEntry(t *testing.T) {
	entry := &zeroconf.ServiceEntry{
		ServiceRecord: zeroconf.ServiceRecord{Instance: "Living Room TV"},
		HostName: "living-room.local",
		Port:     8009,
		AddrIPv4: []net.IP{net.ParseIP("192.168.1.50")},
		Text:     []string{"id=abc123", "fn=Living Room TV", "md=Chromecast", "ca=264709"},
	}

	dev := parseServiceEntry(entry)
	if dev == nil {
		t.Fatal("expected a parsed device")
	}
	if dev.ID != "abc123" {
		t.Errorf("ID = %q, want abc123", dev.ID)
	}
	if dev.FriendlyName != "Living Room TV" {
		t.Errorf("FriendlyName = %q, want Living Room TV", dev.FriendlyName)
	}
	if dev.ModelName != "Chromecast" {
		t.Errorf("ModelName = %q, want Chromecast", dev.ModelName)
	}
	if dev.Port != 8009 {
		t.Errorf("Port = %d, want 8009", dev.Port)
	}
	if !dev.IP.Equal(net.ParseIP("192.168.1.50")) {
		t.Errorf("IP = %v, want 192.168.1.50", dev.IP)
	}
}

func TestParseServiceEntryDefaults(t *testing.T) {
	entry := &zeroconf.ServiceEntry{
		ServiceRecord: zeroconf.ServiceRecord{Instance: "Kitchen"},
		Port:     8009,
		AddrIPv4: []net.IP{net.ParseIP("192.168.1.51")},
		Text:     []string{"id=xyz"},
	}

	dev := parseServiceEntry(entry)
	if dev == nil {
		t.Fatal("expected a parsed device")
	}
	if dev.FriendlyName != "Kitchen" {
		t.Errorf("FriendlyName = %q, want Kitchen (from instance)", dev.FriendlyName)
	}
	if dev.ModelName != "Chromecast" {
		t.Errorf("ModelName = %q, want default Chromecast", dev.ModelName)
	}
}

func TestParseServiceEntryNoIP(t *testing.T) {
	entry := &zeroconf.ServiceEntry{
		ServiceRecord: zeroconf.ServiceRecord{Instance: "Ghost"},
		Port:     8009,
		Text:     []string{"id=ghost"},
	}

	if dev := parseServiceEntry(entry); dev != nil {
		t.Errorf("expected nil for entry without IP, got %+v", dev)
	}
}
