package netutil

import (
	"net"
	"testing"
)

func TestDetectIPForTarget(t *testing.T) {
	// Nil target IP should fail
	if _, err := DetectIPForTarget(nil); err == nil {
		t.Fatal("Expected error for nil target IP, got nil")
	}

	// Loopback target should resolve to loopback address
	localIP, err := DetectIPForTarget(net.ParseIP("127.0.0.1"))
	if err != nil {
		t.Fatalf("DetectIPForTarget(127.0.0.1) failed: %v", err)
	}
	if !localIP.IsLoopback() {
		t.Fatalf("Expected loopback IP for loopback target, got %s", localIP.String())
	}
}
