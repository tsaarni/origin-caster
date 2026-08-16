package castrelay

import (
	"crypto/tls"
	"encoding/json"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/tsaarni/origin-caster/internal/castproto"
	"github.com/tsaarni/origin-caster/internal/certs"
	"github.com/tsaarni/origin-caster/internal/mdns"
	"github.com/tsaarni/origin-caster/internal/proxy"
)

func TestInterceptLoadMessage(t *testing.T) {
	proxy := proxy.NewServer("http://192.168.1.100:8888")
	server := &RelayServer{
		httpProxy: proxy,
	}
	sess := &Session{
		relay: server,
	}

	rawPayload := `{
		"type": "LOAD",
		"requestId": 10,
		"media": {
			"contentId": "https://secure-stream.example.com/live/index.m3u8",
			"contentType": "application/x-mpegurl",
			"customData": {
				"referer": "https://secure-stream.example.com/player",
				"origin": "https://secure-stream.example.com"
			}
		},
		"autoplay": true
	}`

	rewritten, ok := sess.interceptLoadMessage(rawPayload)
	if !ok {
		t.Fatal("Expected load message to be intercepted")
	}

	var parsed map[string]interface{}
	if err := json.Unmarshal([]byte(rewritten), &parsed); err != nil {
		t.Fatalf("Failed to parse rewritten JSON: %v", err)
	}

	media := parsed["media"].(map[string]interface{})
	newContentId := media["contentId"].(string)

	if !strings.HasPrefix(newContentId, "http://192.168.1.100:8888/proxy?origin=https%3A%2F%2Fsecure-stream.example.com&referer=https%3A%2F%2Fsecure-stream.example.com%2Fplayer&url=https%3A%2F%2Fsecure-stream.example.com%2Flive%2Findex.m3u8") {
		t.Errorf("Unexpected rewritten contentId: %s", newContentId)
	}
}

func TestRelayEndToEnd(t *testing.T) {
	// 1. Mock Physical Chromecast TLS Server
	mockCert, err := certs.GenerateCastCertificate(net.ParseIP("127.0.0.1"))
	if err != nil {
		t.Fatalf("Failed to create mock cert: %v", err)
	}
	mockTLSConfig := &tls.Config{
		Certificates: []tls.Certificate{mockCert},
	}
	mockListener, err := tls.Listen("tcp", "127.0.0.1:0", mockTLSConfig)
	if err != nil {
		t.Fatalf("Failed to start mock TV listener: %v", err)
	}
	defer mockListener.Close()

	mockTVAddr := mockListener.Addr().(*net.TCPAddr)

	// Channel to signal what mock TV received
	tvReceivedLoad := make(chan string, 1)

	go func() {
		conn, err := mockListener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()

		for {
			msg, err := castproto.ReadFramedMessage(conn)
			if err != nil {
				return
			}
			if msg.Namespace == castproto.NamespaceHeartbeat {
				// Respond PONG
				pong := castproto.NewStringMessage("receiver-0", msg.SourceId, castproto.NamespaceHeartbeat, `{"type":"PONG"}`)
				_ = castproto.WriteFramedMessage(conn, pong)
			}
			if msg.Namespace == castproto.NamespaceMedia && msg.PayloadUtf8 != nil {
				tvReceivedLoad <- *msg.PayloadUtf8
				// Respond with MEDIA_STATUS
				status := castproto.NewStringMessage(msg.DestinationId, msg.SourceId, castproto.NamespaceMedia, `{"type":"MEDIA_STATUS","requestId":1,"status":[{"playerState":"PLAYING"}]}`)
				_ = castproto.WriteFramedMessage(conn, status)
			}
		}
	}()

	// 2. Start MITM Relay Server
	targetDevice := &mdns.DiscoveredDevice{
		FriendlyName: "Mock Android TV",
		IP:           net.ParseIP("127.0.0.1"),
		Port:         mockTVAddr.Port,
	}

	proxyServer := proxy.NewServer("http://127.0.0.1:9999")
	relayServer, err := NewRelayServer("127.0.0.1:0", net.ParseIP("127.0.0.1"), targetDevice, proxyServer, true)
	if err != nil {
		t.Fatalf("NewRelayServer failed: %v", err)
	}
	if err := relayServer.Start(); err != nil {
		t.Fatalf("RelayServer.Start failed: %v", err)
	}
	defer relayServer.Close()

	relayAddr := relayServer.listener.Addr().String()

	// 3. Connect as Chrome Sender
	clientTLS := &tls.Config{InsecureSkipVerify: true}
	senderConn, err := tls.Dial("tcp", relayAddr, clientTLS)
	if err != nil {
		t.Fatalf("Sender failed to connect to relay: %v", err)
	}
	defer senderConn.Close()

	// Send Heartbeat PING
	pingMsg := castproto.NewStringMessage("sender-0", "receiver-0", castproto.NamespaceHeartbeat, `{"type":"PING"}`)
	if err := castproto.WriteFramedMessage(senderConn, pingMsg); err != nil {
		t.Fatalf("Failed to send PING: %v", err)
	}

	// Expect PONG
	pongResp, err := castproto.ReadFramedMessage(senderConn)
	if err != nil {
		t.Fatalf("Failed to read PONG: %v", err)
	}
	if pongResp.PayloadUtf8 == nil || *pongResp.PayloadUtf8 != `{"type":"PONG"}` {
		t.Errorf("Expected PONG response, got: %v", pongResp.PayloadUtf8)
	}

	// Send LOAD request
	loadJSON := `{"type":"LOAD","requestId":1,"media":{"contentId":"https://cdn.example.com/video.m3u8"}}`
	loadMsg := castproto.NewStringMessage("sender-0", "web-1", castproto.NamespaceMedia, loadJSON)
	if err := castproto.WriteFramedMessage(senderConn, loadMsg); err != nil {
		t.Fatalf("Failed to send LOAD: %v", err)
	}

	// Verify TV received rewritten LOAD
	select {
	case tvPayload := <-tvReceivedLoad:
		if !strings.Contains(tvPayload, "http://127.0.0.1:9999/proxy?origin=https%3A%2F%2Fcdn.example.com&referer=https%3A%2F%2Fcdn.example.com%2F&url=https%3A%2F%2Fcdn.example.com%2Fvideo.m3u8") {
			t.Errorf("TV received unexpected rewritten LOAD payload: %s", tvPayload)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Timeout waiting for TV to receive intercepted LOAD")
	}

	// Verify Sender received MEDIA_STATUS back
	statusMsg, err := castproto.ReadFramedMessage(senderConn)
	if err != nil {
		t.Fatalf("Sender failed to read MEDIA_STATUS: %v", err)
	}
	if statusMsg.PayloadUtf8 == nil || !strings.Contains(*statusMsg.PayloadUtf8, "MEDIA_STATUS") {
		t.Errorf("Expected MEDIA_STATUS, got: %v", statusMsg.PayloadUtf8)
	}
}

func TestDetectContentType(t *testing.T) {
	tests := []struct {
		url      string
		expected string
	}{
		{"https://cdn.example.com/streamsvr/o-gKcDZd5z/1-33?e=xyz", "application/x-mpegurl"},
		{"https://example.com/stream/playlist.m3u8", "application/x-mpegurl"},
		{"https://example.com/hls/live.m3u8", "application/x-mpegurl"},
		{"https://example.com/video.mpd", "application/dash+xml"},
		{"https://example.com/video.mp4", "video/mp4"},
		{"https://example.com/video.webm", "video/webm"},
	}

	for _, tt := range tests {
		got := detectContentType(tt.url, nil)
		if got != tt.expected {
			t.Errorf("detectContentType(%q) = %q, want %q", tt.url, got, tt.expected)
		}
	}
}

func TestNormalizeContentType(t *testing.T) {
	tests := []struct {
		rawType  string
		url      string
		expected string
	}{
		{"hls", "https://cdn.example.com/streamsvr/xyz", "application/x-mpegurl"},
		{"HLS", "https://cdn.example.com/streamsvr/xyz", "application/x-mpegurl"},
		{"m3u8", "https://cdn.example.com/streamsvr/xyz", "application/x-mpegurl"},
		{"application/vnd.apple.mpegurl", "https://cdn.example.com/streamsvr/xyz", "application/x-mpegurl"},
		{"dash", "https://example.com/manifest.mpd", "application/dash+xml"},
		{"mp4", "https://example.com/video.mp4", "video/mp4"},
		{"", "https://cdn.example.com/streamsvr/xyz", "application/x-mpegurl"},
	}

	for _, tt := range tests {
		got := normalizeContentType(tt.rawType, tt.url, nil)
		if got != tt.expected {
			t.Errorf("normalizeContentType(%q, %q) = %q, want %q", tt.rawType, tt.url, got, tt.expected)
		}
	}
}
