package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// CastRequest holds the payload passed to /api/cast from the browser console
// or an API client. The web server parses it and the castrelay controller
// dispatches it to the physical TV.
type CastRequest struct {
	URL         string            `json:"url"`
	Title       string            `json:"title,omitempty"`
	Origin      string            `json:"origin,omitempty"`
	Referer     string            `json:"referer,omitempty"`
	Cookies     string            `json:"cookies,omitempty"`
	UserAgent   string            `json:"userAgent,omitempty"`
	CurrentTime float64           `json:"currentTime,omitempty"`
	ContentType string            `json:"contentType,omitempty"`
	Headers     map[string]string `json:"headers,omitempty"`
	RawHeaders  string            `json:"rawHeaders,omitempty"`
}

// PlaybackState represents current media playback status on the physical TV.
type PlaybackState struct {
	MediaSessionID int                    `json:"mediaSessionId"`
	PlayerState    string                 `json:"playerState"` // "PLAYING", "PAUSED", "BUFFERING", "IDLE"
	CurrentTime    float64                `json:"currentTime"`
	Duration       float64                `json:"duration"`
	VolumeLevel    float64                `json:"volumeLevel"`
	Muted          bool                   `json:"muted"`
	ContentID      string                 `json:"contentId"`
	Title          string                 `json:"title"`
	LastUpdate     time.Time              `json:"lastUpdate"`
	ActiveApp      string                 `json:"activeApp"`
	ReceiverName   string                 `json:"receiverName,omitempty"`
	ReceiverModel  string                 `json:"receiverModel,omitempty"`
	ReceiverIP     string                 `json:"receiverIP,omitempty"`
	CustomData     map[string]interface{} `json:"customData,omitempty"`
}

// Stats tracks media proxy metrics.
type Stats struct {
	TotalRequests    uint64 `json:"total_requests"`
	TotalBytesServed uint64 `json:"total_bytes_served"`
	ActiveStreams    int64  `json:"active_streams"`
	M3U8Rewrites     uint64 `json:"m3u8_rewrites"`
}

// Server fetches media files from the upstream host on behalf of the physical
// TV. It injects the browser's credentials (cookies, referer, origin,
// user-agent) into every request and rewrites HLS playlists so the TV keeps
// fetching through the proxy.
type Server struct {
	baseURL       string // public address, e.g. "http://192.168.1.129:8888"
	client        *http.Client
	stats         Stats
	mu            sync.RWMutex
	activeSession *CastRequest
}

// NewServer creates a new media proxy instance. baseURL is the public address
// of this server (e.g. "http://localhost:8888" or "http://192.168.1.50:8888")
// and is embedded in rewritten HLS playlists.
func NewServer(baseURL string) *Server {
	return &Server{
		baseURL: baseURL,
		client: &http.Client{
			Timeout: 45 * time.Second,
			Transport: &http.Transport{
				MaxIdleConns:        100,
				MaxIdleConnsPerHost: 20,
				IdleConnTimeout:     90 * time.Second,
				DisableCompression:  false,
			},
		},
	}
}

// BaseURL returns the root URL of the proxy server.
func (s *Server) BaseURL() string {
	return s.baseURL
}

// BuildProxyURL generates a proxy URL pointing to the given upstream target URL.
func (s *Server) BuildProxyURL(targetURL, origin, referer, customHeadersJSON string) string {
	q := url.Values{}
	q.Set("url", targetURL)
	if origin != "" {
		q.Set("origin", origin)
	}
	if referer != "" {
		q.Set("referer", referer)
	}
	if customHeadersJSON != "" {
		q.Set("headers", customHeadersJSON)
	}
	return fmt.Sprintf("%s/proxy?%s", s.BaseURL(), q.Encode())
}

// SetActiveSession stores the browser credentials captured by the web server
// (/api/cast) so subsequent /proxy fetches can inject them upstream.
func (s *Server) SetActiveSession(req CastRequest) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.activeSession = &req
}

// ActiveSession returns the most recently captured cast request, or nil.
func (s *Server) ActiveSession() *CastRequest {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.activeSession
}

// Stats returns a snapshot of the proxy's counters.
func (s *Server) Stats() Stats {
	return Stats{
		TotalRequests:    atomic.LoadUint64(&s.stats.TotalRequests),
		TotalBytesServed: atomic.LoadUint64(&s.stats.TotalBytesServed),
		ActiveStreams:    atomic.LoadInt64(&s.stats.ActiveStreams),
		M3U8Rewrites:     atomic.LoadUint64(&s.stats.M3U8Rewrites),
	}
}

// Handler returns the HTTP handler for the media proxy endpoint (/proxy).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/proxy", s.handleProxy)
	return mux
}

// SetCORSHeaders sets permissive CORS headers on the response. Both the media
// proxy and the web server set them so browsers and the TV can fetch
// cross-origin.
func SetCORSHeaders(w http.ResponseWriter) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, HEAD, OPTIONS, POST, PUT, DELETE")
	w.Header().Set("Access-Control-Allow-Headers", "*")
	w.Header().Set("Access-Control-Allow-Private-Network", "true")
	w.Header().Set("Access-Control-Expose-Headers", "Content-Length, Content-Range, Accept-Ranges, Content-Type")
	w.Header().Set("Access-Control-Allow-Credentials", "true")
}

func (s *Server) handleProxy(w http.ResponseWriter, r *http.Request) {
	SetCORSHeaders(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusOK)
		return
	}

	atomic.AddUint64(&s.stats.TotalRequests, 1)

	rawTargetURL := r.URL.Query().Get("url")
	if rawTargetURL == "" {
		http.Error(w, "missing 'url' query parameter", http.StatusBadRequest)
		return
	}

	if !strings.HasPrefix(rawTargetURL, "http://") && !strings.HasPrefix(rawTargetURL, "https://") {
		origin := r.URL.Query().Get("origin")
		referer := r.URL.Query().Get("referer")
		if origin != "" {
			rawTargetURL = strings.TrimRight(origin, "/") + "/" + strings.TrimLeft(rawTargetURL, "/")
		} else if referer != "" {
			if refURL, err := url.Parse(referer); err == nil {
				if resolved, err := refURL.Parse(rawTargetURL); err == nil {
					rawTargetURL = resolved.String()
				}
			}
		}
	}

	targetURL, err := url.Parse(rawTargetURL)
	if err != nil || !targetURL.IsAbs() {
		http.Error(w, "invalid target URL", http.StatusBadRequest)
		return
	}

	// Create upstream request
	upstreamReq, err := http.NewRequestWithContext(r.Context(), r.Method, rawTargetURL, nil)
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to create upstream request: %v", err), http.StatusInternalServerError)
		return
	}

	s.mu.RLock()
	session := s.activeSession
	s.mu.RUnlock()

	// Forward standard headers
	if rangeHdr := r.Header.Get("Range"); rangeHdr != "" {
		upstreamReq.Header.Set("Range", rangeHdr)
	}
	if acceptHdr := r.Header.Get("Accept"); acceptHdr != "" {
		upstreamReq.Header.Set("Accept", acceptHdr)
	}

	// User-Agent
	ua := ""
	if session != nil && session.UserAgent != "" {
		ua = session.UserAgent
	}
	if ua == "" && r.Header.Get("User-Agent") != "" {
		ua = r.Header.Get("User-Agent")
	}
	if ua == "" {
		ua = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/128.0.0.0 Safari/537.36"
	}
	upstreamReq.Header.Set("User-Agent", ua)

	// Origin injection
	origin := r.URL.Query().Get("origin")
	if origin == "" && session != nil && session.Origin != "" {
		origin = session.Origin
	}
	if origin == "" {
		origin = fmt.Sprintf("%s://%s", targetURL.Scheme, targetURL.Host)
	}
	upstreamReq.Header.Set("Origin", origin)

	// Referer injection
	referer := r.URL.Query().Get("referer")
	if referer == "" && session != nil && session.Referer != "" {
		referer = session.Referer
	}
	if referer == "" {
		referer = fmt.Sprintf("%s://%s/", targetURL.Scheme, targetURL.Host)
	}
	upstreamReq.Header.Set("Referer", referer)

	// Cookie injection
	if session != nil && session.Cookies != "" && upstreamReq.Header.Get("Cookie") == "" {
		upstreamReq.Header.Set("Cookie", session.Cookies)
	}

	// Extra custom headers
	if headersJSON := r.URL.Query().Get("headers"); headersJSON != "" {
		var extraHeaders map[string]string
		if jsonErr := json.Unmarshal([]byte(headersJSON), &extraHeaders); jsonErr == nil {
			for k, v := range extraHeaders {
				upstreamReq.Header.Set(k, v)
			}
		}
	} else if session != nil && session.Headers != nil {
		for k, v := range session.Headers {
			upstreamReq.Header.Set(k, v)
		}
	}

	atomic.AddInt64(&s.stats.ActiveStreams, 1)
	defer atomic.AddInt64(&s.stats.ActiveStreams, -1)

	slog.Debug("Proxy handling media request", "upstream", rawTargetURL)

	resp, err := s.client.Do(upstreamReq)
	if err != nil {
		slog.Error("Upstream fetch error", "error", err, "url", rawTargetURL)
		http.Error(w, fmt.Sprintf("upstream request error: %v", err), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	slog.Debug("Upstream response", "status", resp.StatusCode, "contentType", resp.Header.Get("Content-Type"), "length", resp.ContentLength)

	// Check if the response is an HLS M3U8 playlist
	contentType := resp.Header.Get("Content-Type")
	isM3U8 := strings.Contains(strings.ToLower(contentType), "mpegurl") ||
		strings.HasSuffix(strings.ToLower(targetURL.Path), ".m3u8")

	if isM3U8 || resp.StatusCode == http.StatusOK {
		peekBuf := make([]byte, 10)
		n, _ := io.ReadFull(resp.Body, peekBuf)
		combinedReader := io.MultiReader(bytes.NewReader(peekBuf[:n]), resp.Body)

		if bytes.HasPrefix(peekBuf[:n], []byte("#EXTM3U")) || isM3U8 {
			atomic.AddUint64(&s.stats.M3U8Rewrites, 1)
			s.handleM3U8(w, combinedReader, targetURL, origin, referer, r.URL.Query().Get("headers"))
			return
		}

		s.streamMedia(w, resp, combinedReader, targetURL)
		return
	}

	s.streamMedia(w, resp, resp.Body, targetURL)
}

func (s *Server) handleM3U8(w http.ResponseWriter, r io.Reader, baseURL *url.URL, origin, referer, headersJSON string) {
	bodyBytes, err := io.ReadAll(r)
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to read M3U8: %v", err), http.StatusInternalServerError)
		return
	}

	slog.Debug("Original M3U8 manifest", "url", baseURL.String(), "manifest", string(bodyBytes))

	rewritten := RewriteM3U8(string(bodyBytes), baseURL, s.BaseURL(), origin, referer, headersJSON)

	slog.Debug("M3U8 manifest rewritten", "url", baseURL.String(), "length", len(rewritten))

	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(rewritten)))
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(rewritten))
	atomic.AddUint64(&s.stats.TotalBytesServed, uint64(len(rewritten)))
}

func (s *Server) streamMedia(w http.ResponseWriter, upstreamResp *http.Response, body io.Reader, targetURL *url.URL) {
	for _, h := range []string{"Content-Length", "Content-Range", "Accept-Ranges", "Last-Modified", "ETag"} {
		if val := upstreamResp.Header.Get(h); val != "" {
			w.Header().Set(h, val)
		}
	}
	if w.Header().Get("Accept-Ranges") == "" {
		w.Header().Set("Accept-Ranges", "bytes")
	}

	ct := upstreamResp.Header.Get("Content-Type")
	lowerPath := ""
	if targetURL != nil {
		lowerPath = strings.ToLower(targetURL.Path)
	}

	peekBuf := make([]byte, 16)
	n, _ := io.ReadFull(body, peekBuf)
	combinedBody := io.MultiReader(bytes.NewReader(peekBuf[:n]), body)

	isMPEGTS := n > 0 && peekBuf[0] == 0x47
	isMP4 := n >= 8 && (string(peekBuf[4:8]) == "ftyp" || string(peekBuf[4:8]) == "moov" || string(peekBuf[4:8]) == "mdat")

	// If upstream sends image/png, image/jpeg, or generic stream for a video chunk (.png, .ts, .m4s), normalize to video/mp2t
	if isMPEGTS || strings.HasSuffix(lowerPath, ".png") || strings.HasSuffix(lowerPath, ".ts") || strings.HasSuffix(lowerPath, ".m4s") {
		if strings.Contains(ct, "image/") || strings.Contains(ct, "text/") || ct == "" || strings.Contains(ct, "octet-stream") || isMPEGTS {
			ct = "video/mp2t"
		}
	} else if isMP4 || strings.HasSuffix(lowerPath, ".mp4") {
		if strings.Contains(ct, "octet-stream") || ct == "" {
			ct = "video/mp4"
		}
	}

	if ct != "" {
		w.Header().Set("Content-Type", ct)
	}

	w.WriteHeader(upstreamResp.StatusCode)

	buf := make([]byte, 64*1024)
	var written uint64
	for {
		nr, er := combinedBody.Read(buf)
		if nr > 0 {
			nw, ew := w.Write(buf[0:nr])
			if nw < 0 || nr < nw {
				nw = 0
				if ew == nil {
					ew = fmt.Errorf("invalid write result")
				}
			}
			written += uint64(nw)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			if ew != nil {
				break
			}
			if nr != nw {
				break
			}
		}
		if er != nil {
			break
		}
	}
	atomic.AddUint64(&s.stats.TotalBytesServed, written)
}


