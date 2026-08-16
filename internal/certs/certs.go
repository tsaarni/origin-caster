package certs

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"

	"github.com/tsaarni/certyaml"
)

// GenerateCastCertificate generates a TLS certificate using the certyaml package with RSA 2048-bit keys,
// matching the Google Cast V2 receiver specification expected by Chrome.
func GenerateCastCertificate(ip net.IP) (tls.Certificate, error) {
	sans := []string{
		"DNS:localhost",
		"DNS:*.local",
		"DNS:chromecast.local",
		"IP:127.0.0.1",
		"IP:::1",
	}
	if ip != nil && !ip.IsLoopback() {
		sans = append(sans, "IP:"+ip.String())
	}

	isCA := false
	certDef := certyaml.Certificate{
		Subject:         "cn=Cast Device, o=Google Inc.",
		SubjectAltNames: sans,
		KeyType:         certyaml.KeyTypeRSA,
		KeySize:         2048,
		IsCA:            &isCA,
		KeyUsage:        x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:     []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}

	tlsCert, err := certDef.TLSCertificate()
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("certyaml failed to generate TLS certificate: %w", err)
	}

	return tlsCert, nil
}

// NewServerTLSConfig builds a tls.Config suitable for acting as a Chromecast V2 receiver server.
func NewServerTLSConfig(ip net.IP) (*tls.Config, error) {
	cert, err := GenerateCastCertificate(ip)
	if err != nil {
		return nil, err
	}

	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS10,
	}, nil
}

// NewClientTLSConfig builds a tls.Config suitable for dialing the physical Chromecast receiver.
func NewClientTLSConfig() *tls.Config {
	return &tls.Config{
		InsecureSkipVerify: true,
		MinVersion:         tls.VersionTLS10,
	}
}
