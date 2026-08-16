package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tsaarni/origin-caster/internal/proxy"
)

func TestAppJSServesGeneratedSnippet(t *testing.T) {
	s := NewServer(proxy.NewServer("http://localhost:8888"))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/app.js", nil))

	body := rec.Body.String()
	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	if strings.Contains(body, "/*__SNIPPET__*/") {
		t.Fatal("placeholder not substituted")
	}
	if !strings.Contains(body, "ONELINER_SNIPPET_CORE = '(function(){") {
		t.Fatal("minified snippet not injected")
	}
	if !strings.Contains(body, "__BASE__/api/cast?") {
		t.Fatal("base placeholder missing from snippet")
	}
	// the injected snippet must be a single line (no newline before the IIFE body)
	if strings.Contains(body, "ONELINER_SNIPPET_CORE = '(function(){\n") {
		t.Fatal("snippet should be single-line")
	}
}

func TestSnippetEndpointRemoved(t *testing.T) {
	s := NewServer(proxy.NewServer("http://localhost:8888"))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/snippet.js", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (no duplicate snippet endpoint)", rec.Code)
	}
}
