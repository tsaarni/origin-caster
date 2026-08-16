package proxy

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestRewriteMasterM3U8(t *testing.T) {
	masterPlaylist := `#EXTM3U
#EXT-X-VERSION:3
#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="audio",NAME="English",DEFAULT=YES,AUTOSELECT=YES,URI="audio/eng.m3u8"
#EXT-X-STREAM-INF:BANDWIDTH=1280000,RESOLUTION=720x480
720p/prog_index.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=2560000,RESOLUTION=1280x720
1080p/prog_index.m3u8
#EXT-X-I-FRAME-STREAM-INF:BANDWIDTH=86000,URI="iframe.m3u8"
`
	baseURL, _ := url.Parse("https://video.example.com/live/master.m3u8")
	proxyBase := "http://192.168.1.100:8888"
	origin := "https://video.example.com"
	referer := "https://video.example.com/watch/123"

	rewritten := RewriteM3U8(masterPlaylist, baseURL, proxyBase, origin, referer, "")

	if !strings.Contains(rewritten, "http://192.168.1.100:8888/proxy?origin=https%3A%2F%2Fvideo.example.com&referer=https%3A%2F%2Fvideo.example.com%2Fwatch%2F123&url=https%3A%2F%2Fvideo.example.com%2Flive%2F720p%2Fprog_index.m3u8") {
		t.Errorf("Expected rewritten 720p stream URL, got:\n%s", rewritten)
	}

	if !strings.Contains(rewritten, `URI="http://192.168.1.100:8888/proxy?origin=https%3A%2F%2Fvideo.example.com&referer=https%3A%2F%2Fvideo.example.com%2Fwatch%2F123&url=https%3A%2F%2Fvideo.example.com%2Flive%2Faudio%2Feng.m3u8"`) {
		t.Errorf("Expected rewritten audio URI, got:\n%s", rewritten)
	}

	if !strings.Contains(rewritten, `URI="http://192.168.1.100:8888/proxy?origin=https%3A%2F%2Fvideo.example.com&referer=https%3A%2F%2Fvideo.example.com%2Fwatch%2F123&url=https%3A%2F%2Fvideo.example.com%2Flive%2Fiframe.m3u8"`) {
		t.Errorf("Expected rewritten iframe URI, got:\n%s", rewritten)
	}
}

func TestRewriteMediaM3U8WithKeysAndSegments(t *testing.T) {
	mediaPlaylist := `#EXTM3U
#EXT-X-VERSION:3
#EXT-X-TARGETDURATION:10
#EXT-X-MEDIA-SEQUENCE:0
#EXT-X-KEY:METHOD=AES-128,URI="keys/key.bin",IV=0x1234567890abcdef1234567890abcdef
#EXT-X-MAP:URI="init.mp4"
#EXTINF:9.009,
segment_000.ts
#EXTINF:9.009,
https://cdn.example.com/segments/segment_001.ts?token=abc
#EXT-X-ENDLIST
`
	baseURL, _ := url.Parse("https://media.example.com/vod/stream.m3u8")
	proxyBase := "http://192.168.1.100:8888"

	rewritten := RewriteM3U8(mediaPlaylist, baseURL, proxyBase, "https://media.example.com", "https://media.example.com/play", "")

	// Verify key URI rewritten
	if !strings.Contains(rewritten, `URI="http://192.168.1.100:8888/proxy?origin=https%3A%2F%2Fmedia.example.com&referer=https%3A%2F%2Fmedia.example.com%2Fplay&url=https%3A%2F%2Fmedia.example.com%2Fvod%2Fkeys%2Fkey.bin"`) {
		t.Errorf("Expected rewritten KEY URI, got:\n%s", rewritten)
	}

	// Verify init map rewritten
	if !strings.Contains(rewritten, `URI="http://192.168.1.100:8888/proxy?origin=https%3A%2F%2Fmedia.example.com&referer=https%3A%2F%2Fmedia.example.com%2Fplay&url=https%3A%2F%2Fmedia.example.com%2Fvod%2Finit.mp4"`) {
		t.Errorf("Expected rewritten MAP URI, got:\n%s", rewritten)
	}

	// Verify relative segment rewritten
	if !strings.Contains(rewritten, "http://192.168.1.100:8888/proxy?origin=https%3A%2F%2Fmedia.example.com&referer=https%3A%2F%2Fmedia.example.com%2Fplay&url=https%3A%2F%2Fmedia.example.com%2Fvod%2Fsegment_000.ts") {
		t.Errorf("Expected rewritten relative segment URL, got:\n%s", rewritten)
	}

	// Verify absolute segment rewritten
	if !strings.Contains(rewritten, "http://192.168.1.100:8888/proxy?origin=https%3A%2F%2Fmedia.example.com&referer=https%3A%2F%2Fmedia.example.com%2Fplay&url=https%3A%2F%2Fcdn.example.com%2Fsegments%2Fsegment_001.ts%3Ftoken%3Dabc") {
		t.Errorf("Expected rewritten absolute segment URL, got:\n%s", rewritten)
	}
}

func TestProxyServerEndToEnd(t *testing.T) {
	// Upstream test server
	var capturedReferer, capturedOrigin string
	upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedReferer = r.Header.Get("Referer")
		capturedOrigin = r.Header.Get("Origin")

		if strings.HasSuffix(r.URL.Path, "playlist.m3u8") {
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			_, _ = w.Write([]byte("#EXTM3U\n#EXTINF:10.0,\nseg1.ts\n"))
			return
		}

		if strings.HasSuffix(r.URL.Path, "seg1.ts") {
			w.Header().Set("Content-Type", "video/mp2t")
			if r.Header.Get("Range") != "" {
				w.Header().Set("Content-Range", "bytes 0-9/10")
				w.WriteHeader(http.StatusPartialContent)
			} else {
				w.WriteHeader(http.StatusOK)
			}
			_, _ = w.Write([]byte("0123456789"))
			return
		}

		http.NotFound(w, r)
	}))
	defer upstreamServer.Close()

	proxy := NewServer("http://127.0.0.1:8888")
	proxyHandler := proxy.Handler()
	testProxyServer := httptest.NewServer(proxyHandler)
	defer testProxyServer.Close()

	// Point the proxy's advertised base URL at the test server
	proxy.baseURL = testProxyServer.URL

	// 1. Test fetching M3U8 playlist
	playlistTarget := upstreamServer.URL + "/playlist.m3u8"
	reqURL := fmt.Sprintf("%s/proxy?url=%s&referer=%s&origin=%s",
		testProxyServer.URL,
		url.QueryEscape(playlistTarget),
		url.QueryEscape("https://custom-referer.example.com/video"),
		url.QueryEscape("https://custom-referer.example.com"))

	resp, err := http.Get(reqURL)
	if err != nil {
		t.Fatalf("Failed to request proxy: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("Expected 200 OK, got %d", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	bodyStr := string(body)

	if !strings.Contains(bodyStr, "/proxy?") || !strings.Contains(bodyStr, "url=") {
		t.Errorf("Expected rewritten segment in M3U8, got:\n%s", bodyStr)
	}

	if capturedReferer != "https://custom-referer.example.com/video" {
		t.Errorf("Expected referer injected to upstream, got %s", capturedReferer)
	}
	if capturedOrigin != "https://custom-referer.example.com" {
		t.Errorf("Expected origin injected to upstream, got %s", capturedOrigin)
	}

	// 2. Test Range request for segment
	segTarget := upstreamServer.URL + "/seg1.ts"
	segReqURL := fmt.Sprintf("%s/proxy?url=%s", testProxyServer.URL, url.QueryEscape(segTarget))
	segReq, _ := http.NewRequest("GET", segReqURL, nil)
	segReq.Header.Set("Range", "bytes=0-9")

	segResp, err := http.DefaultClient.Do(segReq)
	if err != nil {
		t.Fatalf("Failed to fetch segment: %v", err)
	}
	defer segResp.Body.Close()

	if segResp.StatusCode != http.StatusPartialContent {
		t.Errorf("Expected 206 Partial Content, got %d", segResp.StatusCode)
	}
	if segResp.Header.Get("Access-Control-Allow-Origin") != "*" {
		t.Errorf("Expected CORS headers set to *, got %s", segResp.Header.Get("Access-Control-Allow-Origin"))
	}
}

func TestDisguisedPNGChunkNormalization(t *testing.T) {
	// Upstream test server serving disguised MPEG-TS chunk with image/png content type
	upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		// Byte 0 is 0x47 (MPEG-TS sync byte)
		chunkData := []byte{0x47, 0x40, 0x11, 0x10, 0x00, 0x42, 0xf0, 0x64}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(chunkData)
	}))
	defer upstreamServer.Close()

	proxy := NewServer("http://127.0.0.1:8888")
	testProxyServer := httptest.NewServer(proxy.Handler())
	defer testProxyServer.Close()

	// Point the proxy's advertised base URL at the test server
	proxy.baseURL = testProxyServer.URL

	chunkTarget := upstreamServer.URL + "/o-gKcDZd5z000.png"
	reqURL := fmt.Sprintf("%s/proxy?url=%s", testProxyServer.URL, url.QueryEscape(chunkTarget))

	resp, err := http.Get(reqURL)
	if err != nil {
		t.Fatalf("Failed to fetch chunk: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("Expected 200 OK, got %d", resp.StatusCode)
	}

	if ct := resp.Header.Get("Content-Type"); ct != "video/mp2t" {
		t.Errorf("Expected Content-Type normalized to video/mp2t, got %q", ct)
	}
}
