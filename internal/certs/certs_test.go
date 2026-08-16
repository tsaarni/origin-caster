package certs

import (
	"net"
	"testing"
)

func TestCertGeneration(t *testing.T) {
	ip := net.ParseIP("192.168.1.50")
	cfg, err := NewServerTLSConfig(ip)
	if err != nil {
		t.Fatalf("NewServerTLSConfig failed: %v", err)
	}
	if len(cfg.Certificates) == 0 {
		t.Fatal("Expected at least one certificate")
	}

	clientCfg := NewClientTLSConfig()
	if !clientCfg.InsecureSkipVerify {
		t.Error("Client config should have InsecureSkipVerify true")
	}
}
