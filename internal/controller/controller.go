package controller

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tsaarni/origin-caster/internal/castproto"
	"github.com/tsaarni/origin-caster/internal/mdns"
	"github.com/tsaarni/origin-caster/internal/proxy"
)

// DeviceController provides direct programmatic control of the physical Chromecast receiver.
type DeviceController struct {
	target       *mdns.DiscoveredDevice
	httpProxy    *proxy.Server
	clientTLS    *tls.Config
	conn         net.Conn
	mu           sync.Mutex
	transportID  string
	sessionID    string
	mediaSession int
	state        proxy.PlaybackState
	reqCounter   int64
	done         chan struct{}
	closed       int32
}

// NewDeviceController creates a controller instance for a target physical Chromecast.
func NewDeviceController(target *mdns.DiscoveredDevice, proxy *proxy.Server) *DeviceController {
	return &DeviceController{
		target:    target,
		httpProxy: proxy,
		clientTLS: &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS10},
		done:      make(chan struct{}),
	}
}

// GetStatus returns the current media playback state.
func (c *DeviceController) GetStatus() proxy.PlaybackState {
	c.mu.Lock()
	defer c.mu.Unlock()
	state := c.state
	if c.target != nil {
		state.ReceiverName = c.target.FriendlyName
		state.ReceiverModel = c.target.ModelName
		state.ReceiverIP = c.target.IP.String()
	}
	return state
}

func (c *DeviceController) nextRequestID() int {
	return int(atomic.AddInt64(&c.reqCounter, 1))
}

const controllerSenderID = "sender-ctl"

// EnsureConnection connects to the physical TV if not already connected.
func (c *DeviceController) EnsureConnection() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.conn != nil {
		return nil
	}

	if c.target == nil {
		return errors.New("no target device configured")
	}

	targetAddr := fmt.Sprintf("%s:%d", c.target.IP.String(), c.target.Port)
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	conn, err := tls.DialWithDialer(dialer, "tcp", targetAddr, c.clientTLS)
	if err != nil {
		return fmt.Errorf("failed to dial physical receiver %s: %w", targetAddr, err)
	}
	c.conn = conn

	// Connect to receiver-0
	connectMsg := castproto.NewStringMessage(controllerSenderID, "receiver-0", castproto.NamespaceConnection, `{"type":"CONNECT"}`)
	if err := castproto.WriteFramedMessage(c.conn, connectMsg); err != nil {
		_ = c.conn.Close()
		c.conn = nil
		return fmt.Errorf("failed to send CONNECT: %w", err)
	}

	go c.listenLoop()
	go c.heartbeatLoop()

	return nil
}

func (c *DeviceController) listenLoop() {
	for {
		select {
		case <-c.done:
			return
		default:
		}

		c.mu.Lock()
		conn := c.conn
		c.mu.Unlock()

		if conn == nil {
			return
		}

		msg, err := castproto.ReadFramedMessage(conn)
		if err != nil {
			slog.Debug("TV connection read error", "error", err)
			c.mu.Lock()
			if c.conn != nil {
				_ = c.conn.Close()
				c.conn = nil
			}
			c.mu.Unlock()
			return
		}

		c.handleIncomingMessage(msg)
	}
}

func (c *DeviceController) heartbeatLoop() {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-c.done:
			return
		case <-ticker.C:
			c.mu.Lock()
			conn := c.conn
			tid := c.transportID
			c.mu.Unlock()
			if conn != nil {
				// Heartbeat PING
				ping := castproto.NewStringMessage(controllerSenderID, "receiver-0", castproto.NamespaceHeartbeat, `{"type":"PING"}`)
				_ = castproto.WriteFramedMessage(conn, ping)

				// Poll media status if app transport is active
				if tid != "" {
					reqID := c.nextRequestID()
					mediaStatusReq := fmt.Sprintf(`{"type":"GET_STATUS","requestId":%d}`, reqID)
					msg := castproto.NewStringMessage(controllerSenderID, tid, castproto.NamespaceMedia, mediaStatusReq)
					_ = castproto.WriteFramedMessage(conn, msg)
				}
			}
		}
	}
}

func (c *DeviceController) handleIncomingMessage(msg *castproto.CastMessage) {
	if msg.PayloadUtf8 == nil {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	var raw map[string]interface{}
	if err := json.Unmarshal([]byte(*msg.PayloadUtf8), &raw); err != nil {
		return
	}

	msgType, _ := raw["type"].(string)
	switch msgType {
	case "RECEIVER_STATUS":
		if status, ok := raw["status"].(map[string]interface{}); ok {
			if vol, ok := status["volume"].(map[string]interface{}); ok {
				if lvl, ok := vol["level"].(float64); ok {
					c.state.VolumeLevel = lvl
				}
				if muted, ok := vol["muted"].(bool); ok {
					c.state.Muted = muted
				}
			}
			if apps, ok := status["applications"].([]interface{}); ok && len(apps) > 0 {
				if app, ok := apps[0].(map[string]interface{}); ok {
					if tid, ok := app["transportId"].(string); ok {
						c.transportID = tid
					}
					if sid, ok := app["sessionId"].(string); ok {
						c.sessionID = sid
					}
					if name, ok := app["displayName"].(string); ok {
						c.state.ActiveApp = name
					}
					slog.Debug("TV Receiver Status", "app", c.state.ActiveApp, "transportId", c.transportID, "sessionId", c.sessionID)
				}
			} else {
				// No applications running (e.g. after STOP) - clear stale state
				c.transportID = ""
				c.sessionID = ""
				c.state.ActiveApp = ""
				slog.Debug("TV Receiver Status", "app", "(none)")
			}
		}

	case "MEDIA_STATUS":
		if statusList, ok := raw["status"].([]interface{}); ok && len(statusList) > 0 {
			if status, ok := statusList[0].(map[string]interface{}); ok {
				if msID, ok := status["mediaSessionId"].(float64); ok {
					c.mediaSession = int(msID)
					c.state.MediaSessionID = int(msID)
				}
				if state, ok := status["playerState"].(string); ok {
					c.state.PlayerState = state
				}
				if ct, ok := status["currentTime"].(float64); ok {
					c.state.CurrentTime = ct
				}
				if media, ok := status["media"].(map[string]interface{}); ok {
					if dur, ok := media["duration"].(float64); ok {
						c.state.Duration = dur
					}
					if cid, ok := media["contentId"].(string); ok {
						c.state.ContentID = cid
					}
					if meta, ok := media["metadata"].(map[string]interface{}); ok {
						if title, ok := meta["title"].(string); ok {
							c.state.Title = title
						}
					}
				}
				c.state.LastUpdate = time.Now()
				slog.Debug("TV Media Status", "playerState", c.state.PlayerState, "currentTime", fmt.Sprintf("%.1fs", c.state.CurrentTime), "mediaSessionId", c.mediaSession)
			}
		}
	}
}


// CastMedia launches Default Media Receiver and streams the given request through the proxy.
func (c *DeviceController) CastMedia(req proxy.CastRequest) error {
	if err := c.EnsureConnection(); err != nil {
		return err
	}

	// Consolidate headers
	headersMap := make(map[string]string)
	if req.Headers != nil {
		for k, v := range req.Headers {
			headersMap[k] = v
		}
	}
	if req.RawHeaders != "" {
		var rawH map[string]string
		if json.Unmarshal([]byte(req.RawHeaders), &rawH) == nil {
			for k, v := range rawH {
				headersMap[k] = v
			}
		}
	}
	if req.Cookies != "" && headersMap["Cookie"] == "" {
		headersMap["Cookie"] = req.Cookies
	}
	if req.UserAgent != "" && headersMap["User-Agent"] == "" {
		headersMap["User-Agent"] = req.UserAgent
	}


	// Resolve relative URLs
	req.URL = proxy.ResolveMediaURL(req.URL, req.Origin, req.Referer)

	// 1. Probe TV for any running app session (we may not know about it on fresh start)
	probeReqID := c.nextRequestID()
	probePayload := fmt.Sprintf(`{"type":"GET_STATUS","requestId":%d}`, probeReqID)
	probeMsg := castproto.NewStringMessage(controllerSenderID, "receiver-0", castproto.NamespaceReceiver, probePayload)
	c.mu.Lock()
	_ = castproto.WriteFramedMessage(c.conn, probeMsg)
	c.mu.Unlock()

	// Wait for RECEIVER_STATUS response to populate sessionID
	for i := 0; i < 10; i++ {
		time.Sleep(100 * time.Millisecond)
		c.mu.Lock()
		sid := c.sessionID
		c.mu.Unlock()
		if sid != "" {
			break
		}
	}

	// Stop any existing session so Android TV launches a fresh visible receiver
	c.mu.Lock()
	existingSID := c.sessionID
	c.mu.Unlock()
	if existingSID != "" {
		slog.Info("Stopping prior receiver session before launch", "sessionId", existingSID)
		stopPayload := fmt.Sprintf(`{"type":"STOP","requestId":%d,"sessionId":"%s"}`, c.nextRequestID(), existingSID)
		stopMsg := castproto.NewStringMessage(controllerSenderID, "receiver-0", castproto.NamespaceReceiver, stopPayload)
		c.mu.Lock()
		_ = castproto.WriteFramedMessage(c.conn, stopMsg)
		c.transportID = ""
		c.sessionID = ""
		c.mu.Unlock()
		// Wait for the STOP to take effect; also wait for RECEIVER_STATUS with empty applications
		for i := 0; i < 15; i++ {
			time.Sleep(200 * time.Millisecond)
			c.mu.Lock()
			sid := c.sessionID
			c.mu.Unlock()
			if sid == "" {
				break
			}
		}
	}

	reqID := c.nextRequestID()

	// 2. Launch Default Media Receiver app ("CC1AD845") with Android TV supportedAppTypes
	launchPayload := fmt.Sprintf(`{"type":"LAUNCH","requestId":%d,"appId":"CC1AD845","supportedAppTypes":["WEB","ANDROID_TV"]}`, reqID)
	launchMsg := castproto.NewStringMessage(controllerSenderID, "receiver-0", castproto.NamespaceReceiver, launchPayload)

	c.mu.Lock()
	c.transportID = ""
	c.sessionID = ""
	err := castproto.WriteFramedMessage(c.conn, launchMsg)
	c.mu.Unlock()
	if err != nil {
		return fmt.Errorf("failed to send LAUNCH: %w", err)
	}

	// Wait up to 30s for transport ID from RECEIVER_STATUS (Android TV can take 15-20s)
	var tid, sid string
	var conn net.Conn
	for i := 0; i < 150; i++ {
		time.Sleep(200 * time.Millisecond)
		c.mu.Lock()
		tid = c.transportID
		sid = c.sessionID
		conn = c.conn
		c.mu.Unlock()
		if tid != "" {
			break
		}
	}

	if tid == "" {
		return fmt.Errorf("timed out waiting for TV to launch Default Media Receiver (30s)")
	}

	slog.Info("Connecting to receiver app transport", "transportId", tid, "sessionId", sid)

	// 3. Send CONNECT to the launched app's transport ID
	appConnect := castproto.NewStringMessage(controllerSenderID, tid, castproto.NamespaceConnection, `{"type":"CONNECT","origin":{}}`)
	_ = castproto.WriteFramedMessage(conn, appConnect)

	// 4. Build proxied URL
	proxyURL := c.httpProxy.BuildProxyURL(req.URL)
	slog.Info("Initiating playback via proxy URL", "url", req.URL, "proxyURL", proxyURL)

	contentType := normalizeContentType(req.ContentType, req.URL, headersMap)

	title := req.Title
	if title == "" {
		title = "Streamed Media"
	}

	loadReqID := c.nextRequestID()
	loadPayload := map[string]interface{}{
		"type":        "LOAD",
		"requestId":   loadReqID,
		"sessionId":   sid,
		"autoplay":    true,
		"currentTime": req.CurrentTime,
		"media": map[string]interface{}{
			"contentId":   proxyURL,
			"streamType":  "BUFFERED",
			"contentType": contentType,
			"metadata": map[string]interface{}{
				"type":         0,
				"metadataType": 0,
				"title":        title,
			},
		},
	}

	loadBytes, err := json.Marshal(loadPayload)
	if err != nil {
		return err
	}

	loadMsg := castproto.NewStringMessage(controllerSenderID, tid, castproto.NamespaceMedia, string(loadBytes))
	c.mu.Lock()
	err = castproto.WriteFramedMessage(c.conn, loadMsg)
	c.mu.Unlock()

	if err != nil {
		return fmt.Errorf("failed to send LOAD: %w", err)
	}

	slog.Info("Successfully sent LOAD to TV receiver", "title", title, "contentType", contentType)
	return nil
}

// normalizeContentType converts shorthand names (hls, dash, mp4) or probed types to standard MIME types.
func normalizeContentType(rawType, targetURL string, headers map[string]string) string {
	t := strings.ToLower(strings.TrimSpace(rawType))
	switch {
	case t == "hls" || t == "m3u8" || strings.Contains(t, "mpegurl") || strings.Contains(t, "apple"):
		return "application/x-mpegurl"
	case t == "dash" || t == "mpd" || strings.Contains(t, "dash"):
		return "application/dash+xml"
	case t == "mp4" || t == "video/mp4":
		return "video/mp4"
	case t == "webm" || t == "video/webm":
		return "video/webm"
	}

	detected := detectContentType(targetURL, headers)
	if detected != "" {
		return detected
	}
	return "application/x-mpegurl"
}

// detectContentType probes the stream URL or uses heuristics to return the standard MIME type for Chromecast LOAD.
func detectContentType(targetURL string, headers map[string]string) string {
	lowerURL := strings.ToLower(targetURL)
	if strings.Contains(lowerURL, ".m3u8") || strings.Contains(lowerURL, "streamsvr") || strings.Contains(lowerURL, "/hls/") || strings.Contains(lowerURL, "playlist") || strings.Contains(lowerURL, "manifest") {
		return "application/x-mpegurl"
	}
	if strings.Contains(lowerURL, ".mpd") {
		return "application/dash+xml"
	}
	if strings.HasSuffix(lowerURL, ".mp4") {
		return "video/mp4"
	}
	if strings.HasSuffix(lowerURL, ".webm") {
		return "video/webm"
	}

	// Active HTTP probe with 2s timeout
	client := &http.Client{
		Timeout: 2 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("stopped after 5 redirects")
			}
			return nil
		},
	}

	req, err := http.NewRequest("GET", targetURL, nil)
	if err != nil {
		return "application/x-mpegurl"
	}
	req.Header.Set("Range", "bytes=0-1024")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/128.0.0.0 Safari/537.36")
	}

	resp, err := client.Do(req)
	if err == nil {
		defer resp.Body.Close()
		ct := strings.ToLower(resp.Header.Get("Content-Type"))
		if strings.Contains(ct, "mpegurl") || strings.Contains(ct, "hls") || strings.Contains(ct, "apple") {
			return "application/x-mpegurl"
		}
		if strings.Contains(ct, "dash+xml") {
			return "application/dash+xml"
		}
		if strings.Contains(ct, "video/mp4") {
			return "video/mp4"
		}
		if strings.Contains(ct, "video/webm") {
			return "video/webm"
		}

		buf := make([]byte, 512)
		n, _ := io.ReadFull(resp.Body, buf)
		if bytes.HasPrefix(buf[:n], []byte("#EXTM3U")) {
			return "application/x-mpegurl"
		}
		if bytes.HasPrefix(buf[:n], []byte("<?xml")) || bytes.Contains(buf[:n], []byte("<MPD")) {
			return "application/dash+xml"
		}
		if n >= 8 && (string(buf[4:8]) == "ftyp" || string(buf[4:8]) == "moov") {
			return "video/mp4"
		}
		if n > 0 && buf[0] == 0x47 {
			return "video/mp2t"
		}
	}

	return "application/x-mpegurl"
}

// Play resumes playback.
func (c *DeviceController) Play() error {
	c.mu.Lock()
	c.state.PlayerState = "PLAYING"
	c.mu.Unlock()
	return c.sendMediaCommand(`{"type":"PLAY"}`)
}

// Pause pauses playback.
func (c *DeviceController) Pause() error {
	c.mu.Lock()
	c.state.PlayerState = "PAUSED"
	c.mu.Unlock()
	return c.sendMediaCommand(`{"type":"PAUSE"}`)
}

// Seek seeks to the target timestamp in seconds.
func (c *DeviceController) Seek(seconds float64) error {
	c.mu.Lock()
	c.state.CurrentTime = seconds
	c.mu.Unlock()
	return c.sendMediaCommand(fmt.Sprintf(`{"type":"SEEK","currentTime":%f}`, seconds))
}

// Stop stops playback.
func (c *DeviceController) Stop() error {
	c.mu.Lock()
	c.state.PlayerState = "IDLE"
	c.state.CurrentTime = 0
	c.mu.Unlock()
	return c.sendMediaCommand(`{"type":"STOP"}`)
}

// SetVolume sets the volume level (0.0 to 1.0) and mute state.
func (c *DeviceController) SetVolume(level float64, muted bool) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.conn == nil {
		return errors.New("not connected")
	}

	c.state.VolumeLevel = level
	c.state.Muted = muted

	reqID := c.nextRequestID()
	payload := fmt.Sprintf(`{"type":"SET_VOLUME","requestId":%d,"volume":{"level":%f,"muted":%t}}`, reqID, level, muted)
	msg := castproto.NewStringMessage(controllerSenderID, "receiver-0", castproto.NamespaceReceiver, payload)
	return castproto.WriteFramedMessage(c.conn, msg)
}

func (c *DeviceController) sendMediaCommand(cmdJSON string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.conn == nil {
		return errors.New("not connected")
	}

	var raw map[string]interface{}
	if err := json.Unmarshal([]byte(cmdJSON), &raw); err != nil {
		return err
	}

	raw["requestId"] = c.nextRequestID()
	if c.mediaSession != 0 {
		raw["mediaSessionId"] = c.mediaSession
	}

	tid := c.transportID
	if tid == "" {
		tid = "web-1"
	}

	b, _ := json.Marshal(raw)
	msg := castproto.NewStringMessage(controllerSenderID, tid, castproto.NamespaceMedia, string(b))
	return castproto.WriteFramedMessage(c.conn, msg)
}

// Close closes controller connections.
func (c *DeviceController) Close() {
	if atomic.CompareAndSwapInt32(&c.closed, 0, 1) {
		close(c.done)
		c.mu.Lock()
		if c.conn != nil {
			_ = c.conn.Close()
			c.conn = nil
		}
		c.mu.Unlock()
	}
}
