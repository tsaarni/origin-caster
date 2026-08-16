package netutil

import (
	"testing"
)

func TestDetectLANIP(t *testing.T) {
	ip, err := DetectLANIP()
	if err != nil {
		t.Logf("DetectLANIP failed (may happen in isolated container/sandbox): %v", err)
		return
	}
	t.Logf("Detected LAN IP: %s", ip.String())
	if ip == nil {
		t.Fatal("Expected non-nil IP")
	}
	if ip.IsLoopback() {
		t.Fatalf("Expected non-loopback IP, got %s", ip.String())
	}
}
