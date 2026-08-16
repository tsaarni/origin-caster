package castrelay

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tsaarni/origin-caster/internal/castproto"
	"github.com/tsaarni/origin-caster/internal/certs"
	"github.com/tsaarni/origin-caster/internal/mdns"
	"github.com/tsaarni/origin-caster/internal/proxy"
)

// SessionStats records information about active and past Cast sessions.
type SessionStats struct {
	ActiveSessions    int64  `json:"active_sessions"`
	TotalSessions     uint64 `json:"total_sessions"`
	InterceptedLoads  uint64 `json:"intercepted_loads"`
	MessagesForwarded uint64 `json:"messages_forwarded"`
}

// RelayServer terminates TLS from Chrome (sender) and proxies to physical TV receiver (receiver).
type RelayServer struct {
	listenAddr     string
	localIP        net.IP
	targetDevice   *mdns.DiscoveredDevice
	httpProxy      *proxy.Server
	serverTLS      *tls.Config
	clientTLS      *tls.Config
	listener       net.Listener
	activeSessions map[string]*Session
	mu             sync.RWMutex
	stats          SessionStats
	closed         int32
	verbose        bool
}

// NewRelayServer initializes TLS MITM keys and certificates for proxying Cast V2 traffic.
func NewRelayServer(listenAddr string, localIP net.IP, target *mdns.DiscoveredDevice, proxy *proxy.Server, verbose bool) (*RelayServer, error) {
	serverTLS, err := certs.NewServerTLSConfig(localIP)
	if err != nil {
		return nil, fmt.Errorf("failed to generate Cast server TLS config: %w", err)
	}

	clientTLS := certs.NewClientTLSConfig()

	return &RelayServer{
		listenAddr:     listenAddr,
		localIP:        localIP,
		targetDevice:   target,
		httpProxy:      proxy,
		serverTLS:      serverTLS,
		clientTLS:      clientTLS,
		activeSessions: make(map[string]*Session),
		verbose:        verbose,
	}, nil
}

// UpdateTargetDevice updates the target physical Chromecast receiver.
func (r *RelayServer) UpdateTargetDevice(target *mdns.DiscoveredDevice) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.targetDevice = target
	slog.Info("Target receiver updated", "name", target.FriendlyName, "ip", target.IP.String(), "port", target.Port)
}

// Start begins listening for incoming Cast connections from Chrome.
func (r *RelayServer) Start() error {
	listener, err := tls.Listen("tcp", r.listenAddr, r.serverTLS)
	if err != nil {
		return fmt.Errorf("failed to bind TLS on %s: %w", r.listenAddr, err)
	}
	r.listener = listener

	slog.Info("Cast V2 MITM server listening", "listenAddr", r.listenAddr)

	go r.acceptLoop()
	return nil
}

// Close terminates the relay server and active sessions.
func (r *RelayServer) Close() error {
	if !atomic.CompareAndSwapInt32(&r.closed, 0, 1) {
		return nil
	}
	if r.listener != nil {
		_ = r.listener.Close()
	}
	r.mu.Lock()
	for _, sess := range r.activeSessions {
		sess.Close()
	}
	r.activeSessions = make(map[string]*Session)
	r.mu.Unlock()
	return nil
}

func (r *RelayServer) acceptLoop() {
	for {
		conn, err := r.listener.Accept()
		if err != nil {
			if atomic.LoadInt32(&r.closed) == 1 {
				return
			}
			slog.Debug("Accept error", "error", err)
			time.Sleep(100 * time.Millisecond)
			continue
		}

		go r.handleClient(conn)
	}
}

func (r *RelayServer) handleClient(senderConn net.Conn) {
	atomic.AddUint64(&r.stats.TotalSessions, 1)
	atomic.AddInt64(&r.stats.ActiveSessions, 1)
	defer atomic.AddInt64(&r.stats.ActiveSessions, -1)

	senderAddr := senderConn.RemoteAddr().String()
	slog.Info("Sender connected", "remoteAddr", senderAddr)

	r.mu.RLock()
	target := r.targetDevice
	r.mu.RUnlock()

	if target == nil {
		slog.Error("No physical Chromecast target device configured")
		_ = senderConn.Close()
		return
	}

	// Connect to physical Chromecast receiver
	targetAddr := fmt.Sprintf("%s:%d", target.IP.String(), target.Port)
	slog.Debug("Connecting to physical receiver", "targetAddr", targetAddr)

	dialer := &net.Dialer{Timeout: 10 * time.Second}
	receiverConn, err := tls.DialWithDialer(dialer, "tcp", targetAddr, r.clientTLS)
	if err != nil {
		slog.Error("Failed to connect to physical receiver", "targetAddr", targetAddr, "error", err)
		_ = senderConn.Close()
		return
	}

	slog.Info("Connected to physical receiver", "name", target.FriendlyName)

	sess := &Session{
		id:           senderAddr,
		senderConn:   senderConn,
		receiverConn: receiverConn,
		relay:        r,
		done:         make(chan struct{}),
	}

	r.mu.Lock()
	r.activeSessions[senderAddr] = sess
	r.mu.Unlock()

	defer func() {
		sess.Close()
		r.mu.Lock()
		delete(r.activeSessions, senderAddr)
		r.mu.Unlock()
		slog.Info("Session terminated", "remoteAddr", senderAddr)
	}()

	sess.Run()
}

// Session coordinates bidirectional message proxying between sender (Chrome) and receiver (TV).
type Session struct {
	id           string
	senderConn   net.Conn
	receiverConn net.Conn
	relay        *RelayServer
	done         chan struct{}
	once         sync.Once
	sendMu       sync.Mutex
	recvMu       sync.Mutex
}

func (s *Session) Close() {
	s.once.Do(func() {
		close(s.done)
		if s.senderConn != nil {
			_ = s.senderConn.Close()
		}
		if s.receiverConn != nil {
			_ = s.receiverConn.Close()
		}
	})
}

func (s *Session) Run() {
	var wg sync.WaitGroup
	wg.Add(2)

	// Sender -> TV pump
	go func() {
		defer wg.Done()
		s.pumpSenderToReceiver()
		s.Close()
	}()

	// TV -> Sender pump
	go func() {
		defer wg.Done()
		s.pumpReceiverToSender()
		s.Close()
	}()

	// Keepalive heartbeat
	go s.heartbeatLoop()

	wg.Wait()
}

func (s *Session) pumpSenderToReceiver() {
	for {
		select {
		case <-s.done:
			return
		default:
		}

		msg, err := castproto.ReadFramedMessage(s.senderConn)
		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
				slog.Debug("Sender read error", "error", err)
			}
			return
		}

		atomic.AddUint64(&s.relay.stats.MessagesForwarded, 1)

		if s.relay.verbose {
			payloadPreview := ""
			if msg.PayloadUtf8 != nil {
				payloadPreview = *msg.PayloadUtf8
				if len(payloadPreview) > 120 {
					payloadPreview = payloadPreview[:120] + "..."
				}
			}
			slog.Debug("SENDER->TV", "namespace", msg.Namespace, "src", msg.SourceId, "dst", msg.DestinationId, "payload", payloadPreview)
		}

		// Handle Heartbeat locally
		if msg.Namespace == castproto.NamespaceHeartbeat && msg.PayloadUtf8 != nil {
			var hb castproto.HeartbeatPayload
			if json.Unmarshal([]byte(*msg.PayloadUtf8), &hb) == nil && hb.Type == "PING" {
				// Respond PONG immediately to sender
				pongPayload := `{"type":"PONG"}`
				pongMsg := castproto.NewStringMessage("receiver-0", msg.SourceId, castproto.NamespaceHeartbeat, pongPayload)
				s.sendMu.Lock()
				_ = castproto.WriteFramedMessage(s.senderConn, pongMsg)
				s.sendMu.Unlock()
				continue
			}
		}

		// Intercept Media LOAD
		if msg.Namespace == castproto.NamespaceMedia && msg.PayloadUtf8 != nil {
			modifiedPayload, intercepted := s.interceptLoadMessage(*msg.PayloadUtf8)
			if intercepted {
				msg.PayloadUtf8 = &modifiedPayload
				atomic.AddUint64(&s.relay.stats.InterceptedLoads, 1)
			}
		}

		// Forward message to physical TV receiver
		s.recvMu.Lock()
		err = castproto.WriteFramedMessage(s.receiverConn, msg)
		s.recvMu.Unlock()
		if err != nil {
			slog.Debug("Forward error to receiver", "error", err)
			return
		}
	}
}

func (s *Session) pumpReceiverToSender() {
	for {
		select {
		case <-s.done:
			return
		default:
		}

		msg, err := castproto.ReadFramedMessage(s.receiverConn)
		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
				slog.Debug("Receiver read error", "error", err)
			}
			return
		}

		atomic.AddUint64(&s.relay.stats.MessagesForwarded, 1)

		if s.relay.verbose {
			payloadPreview := ""
			if msg.PayloadUtf8 != nil {
				payloadPreview = *msg.PayloadUtf8
				if len(payloadPreview) > 120 {
					payloadPreview = payloadPreview[:120] + "..."
				}
			}
			slog.Debug("TV->SENDER", "namespace", msg.Namespace, "src", msg.SourceId, "dst", msg.DestinationId, "payload", payloadPreview)
		}

		// Forward response to Chrome sender
		s.sendMu.Lock()
		err = castproto.WriteFramedMessage(s.senderConn, msg)
		s.sendMu.Unlock()
		if err != nil {
			slog.Debug("Forward error to sender", "error", err)
			return
		}
	}
}

func (s *Session) heartbeatLoop() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	pingPayload := `{"type":"PING"}`
	for {
		select {
		case <-s.done:
			return
		case <-ticker.C:
			// Send PING to TV receiver to keep session active
			tvPing := castproto.NewStringMessage("sender-0", "receiver-0", castproto.NamespaceHeartbeat, pingPayload)
			s.recvMu.Lock()
			_ = castproto.WriteFramedMessage(s.receiverConn, tvPing)
			s.recvMu.Unlock()
		}
	}
}

// interceptLoadMessage parses the media LOAD JSON payload, rewrites contentId to point to local proxy,
// and extracts custom headers/origin/referer.
func (s *Session) interceptLoadMessage(payloadJSON string) (string, bool) {
	var rawMap map[string]interface{}
	if err := json.Unmarshal([]byte(payloadJSON), &rawMap); err != nil {
		return payloadJSON, false
	}

	msgType, _ := rawMap["type"].(string)
	if msgType != "LOAD" {
		return payloadJSON, false
	}

	media, ok := rawMap["media"].(map[string]interface{})
	if !ok || media == nil {
		return payloadJSON, false
	}

	contentID, _ := media["contentId"].(string)
	if contentID == "" {
		return payloadJSON, false
	}

	// If already pointing to our proxy, do not rewrite again
	if s.relay.httpProxy != nil && (len(contentID) >= len(s.relay.httpProxy.BaseURL()) && contentID[:len(s.relay.httpProxy.BaseURL())] == s.relay.httpProxy.BaseURL()) {
		return payloadJSON, false
	}

	slog.Info("Trapped Cast LOAD request", "contentId", contentID)

	// Extract origin / referer / custom headers if present in customData
	var origin, referer string
	var headersJSON string

	if customData, hasCustom := media["customData"].(map[string]interface{}); hasCustom {
		if orig, ok := customData["origin"].(string); ok {
			origin = orig
		}
		if ref, ok := customData["referer"].(string); ok {
			referer = ref
		}
		if hdrs, ok := customData["headers"].(map[string]interface{}); ok {
			strHdrs := make(map[string]string)
			for k, v := range hdrs {
				strHdrs[k] = fmt.Sprintf("%v", v)
			}
			if b, err := json.Marshal(strHdrs); err == nil {
				headersJSON = string(b)
			}
		}
	}

	// Fallback to deriving origin & referer from contentId URL
	if origin == "" || referer == "" {
		if parsedURL, err := url.Parse(contentID); err == nil && parsedURL.Scheme != "" && parsedURL.Host != "" {
			if origin == "" {
				origin = fmt.Sprintf("%s://%s", parsedURL.Scheme, parsedURL.Host)
			}
			if referer == "" {
				referer = fmt.Sprintf("%s://%s/", parsedURL.Scheme, parsedURL.Host)
			}
		}
	}

	// Build local proxy URL
	proxyURL := s.relay.httpProxy.BuildProxyURL(contentID, origin, referer, headersJSON)
	slog.Info("Rewritten contentId to local streaming relay", "proxyURL", proxyURL)

	// Replace contentId
	media["contentId"] = proxyURL

	// Ensure streamType is set
	if media["streamType"] == nil || media["streamType"] == "" {
		media["streamType"] = "BUFFERED"
	}

	// Normalize contentType if needed
	if ct, ok := media["contentType"].(string); !ok || ct == "" || ct == "video/mp4" {
		lowerContentID := strings.ToLower(contentID)
		if strings.Contains(lowerContentID, ".m3u8") || strings.Contains(lowerContentID, "streamsvr") || strings.Contains(lowerContentID, "/hls/") {
			media["contentType"] = "application/x-mpegurl"
		}
	}

	buf := new(bytes.Buffer)
	enc := json.NewEncoder(buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(rawMap); err != nil {
		slog.Error("Error re-marshaling LOAD payload", "error", err)
		return payloadJSON, false
	}

	return strings.TrimSpace(buf.String()), true
}
