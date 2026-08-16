package controller

import "testing"

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
