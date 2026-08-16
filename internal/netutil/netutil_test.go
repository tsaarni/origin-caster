package netutil

import (
	"testing"
)

func TestDetectLANIP(t *testing.T) {
	info, err := DetectLANIP("")
	if err != nil {
		t.Logf("DetectLANIP failed (may happen in isolated container/sandbox): %v", err)
		return
	}
	t.Logf("Detected LAN IP: %s, Interface: %+v", info.IP.String(), info.Interface)
	if info.IP == nil {
		t.Fatal("Expected non-nil IP")
	}
	if info.IP.IsLoopback() {
		t.Fatalf("Expected non-loopback IP, got %s", info.IP.String())
	}
}
