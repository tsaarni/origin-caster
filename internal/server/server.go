package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tsaarni/origin-caster/internal/proxy"
	"github.com/tsaarni/origin-caster/web"
)

// StreamController is implemented by controller.DeviceController.
type StreamController interface {
	CastMedia(req proxy.CastRequest) error
	Play() error
	Pause() error
	Seek(seconds float64) error
	SetVolume(level float64, muted bool) error
	Stop() error
	GetStatus() proxy.PlaybackState
}

// Server is the web dashboard and REST API server. It serves the embedded web
// UI from the web package and mounts the media proxy at /proxy.
type Server struct {
	mediaProxy *proxy.Server
	controller StreamController
	mu         sync.RWMutex
}

// NewServer creates the web server. mediaProxy is the media proxy that will be
// mounted at /proxy.
func NewServer(mediaProxy *proxy.Server) *Server {
	return &Server{mediaProxy: mediaProxy}
}

// SetController attaches the physical Chromecast controller.
func (s *Server) SetController(ctrl StreamController) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.controller = ctrl
}

// Handler returns the full HTTP handler for the dashboard, REST API, and the
// mounted media proxy.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/proxy", s.mediaProxy.Handler())
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/style.css", s.handleStyleCSS)
	mux.HandleFunc("/app.js", s.handleAppJS)
	mux.HandleFunc("/api/cast", s.handleAPICast)
	mux.HandleFunc("/api/play", s.handleAPIPlay)
	mux.HandleFunc("/api/pause", s.handleAPIPause)
	mux.HandleFunc("/api/seek", s.handleAPISeek)
	mux.HandleFunc("/api/volume", s.handleAPIVolume)
	mux.HandleFunc("/api/stop", s.handleAPIStop)
	mux.HandleFunc("/api/stats", s.handleAPIStats)
	mux.HandleFunc("/", s.handleRoot)
	return s.logRequests(mux)
}

// logRequests wraps the mux and logs every incoming HTTP request at debug
// level. It labels the caller as "chromecast" for the media endpoints the TV
// fetches and as "browser" for the web dashboard and REST API. Sensitive
// values (cookies, custom header payloads) are never logged - only their
// presence.
func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		client := "browser"
		switch r.URL.Path {
		case "/proxy":
			client = "chromecast"
		}

		attrs := []any{
			"client", client,
			"method", r.Method,
			"path", r.URL.Path,
			"remote", r.RemoteAddr,
		}
		attrs = append(attrs, s.requestLogParams(r)...)

		slog.Debug("HTTP request", attrs...)
		next.ServeHTTP(w, r)
	})
}

// requestLogParams extracts a safe, whitelisted set of request parameters for
// logging. Params are read from the query string and, for POST requests, from
// the body (JSON or URL-encoded form). Cookie values and custom header payloads
// are never logged, only their presence.
func (s *Server) requestLogParams(r *http.Request) []any {
	whitelist := []string{"url", "title", "origin", "referer", "contentType", "currentTime", "seconds", "delta", "level", "muted"}

	values := url.Values{}
	for _, k := range whitelist {
		if v := r.URL.Query().Get(k); v != "" {
			values.Set(k, v)
		}
	}
	if r.URL.Query().Get("cookies") != "" {
		values.Set("cookies", "present")
	}
	if r.URL.Query().Get("headers") != "" {
		values.Set("headers", "present")
	}

	// Peek at the POST body without consuming it, so the handlers can still read it.
	if r.Method == http.MethodPost && r.Body != nil {
		body, err := io.ReadAll(io.LimitReader(r.Body, 32<<10))
		if err == nil {
			r.Body = io.NopCloser(bytes.NewReader(body))

			payload := url.Values{}
			var m map[string]any
			if json.Unmarshal(body, &m) == nil {
				for k, v := range m {
					if s, ok := v.(string); ok {
						payload.Set(k, s)
					}
				}
			} else if parsed, err := url.ParseQuery(string(body)); err == nil {
				payload = parsed
			}
			for _, k := range whitelist {
				if v := payload.Get(k); v != "" && values.Get(k) == "" {
					values.Set(k, v)
				}
			}
			if payload.Get("cookies") != "" {
				values.Set("cookies", "present")
			}
		}
	}

	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	attrs := make([]any, 0, len(keys)*2)
	for _, k := range keys {
		attrs = append(attrs, k, values.Get(k))
	}
	return attrs
}

func (s *Server) handleStyleCSS(w http.ResponseWriter, r *http.Request) {
	proxy.SetCORSHeaders(w)
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	data, _ := web.Asset("style.css")
	_, _ = w.Write(data)
}

func (s *Server) handleAppJS(w http.ResponseWriter, r *http.Request) {
	proxy.SetCORSHeaders(w)
	w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	data, err := web.Asset("app.js")
	if err != nil {
		http.Error(w, "app.js not found", http.StatusInternalServerError)
		return
	}
	body := string(data)
	if strings.Contains(body, "/*__SNIPPET__*/") {
		snippet, err := minifiedSnippet()
		if err != nil {
			slog.Error("Snippet minification failed", "error", err)
			castScript, aerr := web.Asset("cast.js")
			if aerr != nil {
				http.Error(w, "cast.js not found", http.StatusInternalServerError)
				return
			}
			snippet = string(castScript) // fall back to the readable script (multi-line)
		}
		body = strings.Replace(body, "/*__SNIPPET__*/", escapeJSString(snippet), 1)
	}
	_, _ = w.Write([]byte(body))
}

// escapeJSString escapes a string for embedding in a single-quoted JS string
// literal.
func escapeJSString(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 8)
	for _, r := range s {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '\'':
			b.WriteString(`\'`)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// castRequestFromForm builds a CastRequest from URL-encoded form values.
func castRequestFromForm(r *http.Request) proxy.CastRequest {
	var req proxy.CastRequest
	req.URL = r.FormValue("url")
	req.Title = r.FormValue("title")
	req.Origin = r.FormValue("origin")
	req.Referer = r.FormValue("referer")
	req.Cookies = r.FormValue("cookies")
	req.UserAgent = r.FormValue("userAgent")
	req.ContentType = r.FormValue("contentType")
	req.RawHeaders = r.FormValue("headers")
	if ctStr := r.FormValue("currentTime"); ctStr != "" {
		req.CurrentTime, _ = strconv.ParseFloat(ctStr, 64)
	}
	return req
}

func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/" {
		s.handleDashboard(w, r)
		return
	}
	http.NotFound(w, r)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	proxy.SetCORSHeaders(w)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"ok","time":"` + time.Now().Format(time.RFC3339) + `"}`))
}

func (s *Server) handleAPIStats(w http.ResponseWriter, r *http.Request) {
	proxy.SetCORSHeaders(w)
	w.Header().Set("Content-Type", "application/json")

	s.mu.RLock()
	ctrl := s.controller
	s.mu.RUnlock()

	var playback proxy.PlaybackState
	if ctrl != nil {
		playback = ctrl.GetStatus()
	}

	stats := s.mediaProxy.Stats()
	_ = json.NewEncoder(w).Encode(map[string]any{
		"total_requests": stats.TotalRequests,
		"total_bytes":    stats.TotalBytesServed,
		"active_streams": stats.ActiveStreams,
		"m3u8_rewrites":  stats.M3U8Rewrites,
		"base_url":       s.mediaProxy.BaseURL(),
		"playback":       playback,
		"active_session": s.mediaProxy.ActiveSession(),
	})
}

func (s *Server) handleAPICast(w http.ResponseWriter, r *http.Request) {
	proxy.SetCORSHeaders(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusOK)
		return
	}

	s.mu.RLock()
	ctrl := s.controller
	s.mu.RUnlock()

	if ctrl == nil {
		http.Error(w, "No physical Chromecast controller available", http.StatusServiceUnavailable)
		return
	}

	var req proxy.CastRequest
	if r.Method == http.MethodGet {
		q := r.URL.Query()
		req.URL = q.Get("url")
		req.Title = q.Get("title")
		req.Origin = q.Get("origin")
		req.Referer = q.Get("referer")
		req.Cookies = q.Get("cookies")
		req.UserAgent = q.Get("userAgent")
		req.ContentType = q.Get("contentType")
		req.RawHeaders = q.Get("headers")
		if ctStr := q.Get("currentTime"); ctStr != "" {
			req.CurrentTime, _ = strconv.ParseFloat(ctStr, 64)
		}
	} else {
		bodyBytes, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(bodyBytes, &req); err != nil || req.URL == "" {
			// Body already consumed above; restore it so ParseForm can read it.
			r.Body = io.NopCloser(bytes.NewReader(bodyBytes))
			_ = r.ParseForm()
			req = castRequestFromForm(r)
		}
	}

	// Capture the browser's User-Agent from the request header unless the
	// client explicitly provided one. It is forwarded to the upstream media
	// host on every /proxy fetch, so the site sees a browser, not a Cast device.
	if req.UserAgent == "" {
		req.UserAgent = r.Header.Get("User-Agent")
	}

	if req.URL == "" {
		http.Error(w, "missing 'url' parameter", http.StatusBadRequest)
		return
	}

	s.mediaProxy.SetActiveSession(req)

	if err := ctrl.CastMedia(req); err != nil {
		http.Error(w, fmt.Sprintf("Cast failed: %v", err), http.StatusInternalServerError)
		return
	}

	if r.Method == http.MethodGet {
		if wantsHTML(r) {
			// Popup navigation from the browser snippet: auto-closing confirmation page.
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte("<!DOCTYPE html><html><head><title>origin-caster</title></head><body><p>Casting to TV...</p><script>setTimeout(()=>window.close(),100);</script></body></html>"))
			return
		}
		// Return 1x1 transparent GIF for Image() beacon
		w.Header().Set("Content-Type", "image/gif")
		_, _ = w.Write([]byte("GIF89a\x01\x00\x01\x00\x80\x00\x00\xff\xff\xff\x00\x00\x00!\xf9\x04\x01\x00\x00\x00\x00,\x00\x00\x00\x00\x01\x00\x01\x00\x00\x02\x02D\x01\x00;"))
		return
	}

	// Content negotiation: a browser form navigation (the snippet's popup)
	// sends Accept: text/html and gets an auto-closing confirmation page;
	// API clients (fetch, curl) get JSON.
	if wantsHTML(r) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte("<!DOCTYPE html><html><head><title>origin-caster</title></head><body><p>Casting to TV...</p><script>setTimeout(()=>window.close(),100);</script></body></html>"))
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":  "success",
		"message": "Casting initiated to TV",
		"url":     req.URL,
		"title":   req.Title,
	})
}

// wantsHTML reports whether the request is a browser navigation (form
// submission) rather than an API fetch, based on the Accept header
// (content negotiation). JSON is the default for everything else.
func wantsHTML(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), "text/html")
}

func (s *Server) handleAPIPlay(w http.ResponseWriter, r *http.Request) {
	proxy.SetCORSHeaders(w)
	s.mu.RLock()
	ctrl := s.controller
	s.mu.RUnlock()
	if ctrl != nil {
		_ = ctrl.Play()
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "action": "play"})
}

func (s *Server) handleAPIPause(w http.ResponseWriter, r *http.Request) {
	proxy.SetCORSHeaders(w)
	s.mu.RLock()
	ctrl := s.controller
	s.mu.RUnlock()
	if ctrl != nil {
		_ = ctrl.Pause()
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "action": "pause"})
}

func (s *Server) handleAPIStop(w http.ResponseWriter, r *http.Request) {
	proxy.SetCORSHeaders(w)
	s.mu.RLock()
	ctrl := s.controller
	s.mu.RUnlock()
	if ctrl != nil {
		_ = ctrl.Stop()
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "action": "stop"})
}

func (s *Server) handleAPISeek(w http.ResponseWriter, r *http.Request) {
	proxy.SetCORSHeaders(w)
	s.mu.RLock()
	ctrl := s.controller
	s.mu.RUnlock()

	var targetSec float64
	secStr := r.URL.Query().Get("seconds")
	if secStr == "" {
		_ = r.ParseForm()
		secStr = r.FormValue("seconds")
	}

	if secStr != "" {
		targetSec, _ = strconv.ParseFloat(secStr, 64)
	} else if deltaStr := r.URL.Query().Get("delta"); deltaStr != "" {
		delta, _ := strconv.ParseFloat(deltaStr, 64)
		if ctrl != nil {
			status := ctrl.GetStatus()
			targetSec = status.CurrentTime + delta
			if targetSec < 0 {
				targetSec = 0
			}
			if status.Duration > 0 && targetSec > status.Duration {
				targetSec = status.Duration
			}
		}
	} else if deltaForm := r.FormValue("delta"); deltaForm != "" {
		delta, _ := strconv.ParseFloat(deltaForm, 64)
		if ctrl != nil {
			status := ctrl.GetStatus()
			targetSec = status.CurrentTime + delta
			if targetSec < 0 {
				targetSec = 0
			}
			if status.Duration > 0 && targetSec > status.Duration {
				targetSec = status.Duration
			}
		}
	}

	if ctrl != nil {
		_ = ctrl.Seek(targetSec)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "currentTime": targetSec})
}

func (s *Server) handleAPIVolume(w http.ResponseWriter, r *http.Request) {
	proxy.SetCORSHeaders(w)
	s.mu.RLock()
	ctrl := s.controller
	s.mu.RUnlock()

	lvlStr := r.URL.Query().Get("level")
	if lvlStr == "" {
		_ = r.ParseForm()
		lvlStr = r.FormValue("level")
	}
	lvl, err := strconv.ParseFloat(lvlStr, 64)
	if err != nil && ctrl != nil {
		lvl = ctrl.GetStatus().VolumeLevel
	}

	mutedStr := r.URL.Query().Get("muted")
	if mutedStr == "" {
		mutedStr = r.FormValue("muted")
	}
	muted := mutedStr == "true" || mutedStr == "1"

	if ctrl != nil {
		_ = ctrl.SetVolume(lvl, muted)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "level": lvl, "muted": muted})
}

// handleDashboard serves the embedded web dashboard (index.html).
func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	proxy.SetCORSHeaders(w)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	data, err := web.Asset("index.html")
	if err != nil {
		http.Error(w, "index.html not found", http.StatusInternalServerError)
		return
	}
	_, _ = w.Write(data)
}
