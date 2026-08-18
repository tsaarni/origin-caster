package server

import (
	"encoding/json"
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
	snippet, _ := minifiedSnippet()
	t.Logf("Generated snippet size: %d bytes", len(snippet))
	if !strings.Contains(body, "ONELINER_SNIPPET_CORE = \"") && !strings.Contains(body, "ONELINER_SNIPPET_CORE = '") {
		t.Fatal("minified snippet assignment not injected")
	}
	if !strings.Contains(body, "__BASE__/api/cast?") {
		t.Fatal("base placeholder missing from snippet")
	}

	// Verify that the injected string is a syntactically valid JSON string literal (pure Go)
	idx := strings.Index(body, "const ONELINER_SNIPPET_CORE = ")
	if idx == -1 {
		t.Fatal("ONELINER_SNIPPET_CORE assignment not found")
	}
	stmt := body[idx+len("const ONELINER_SNIPPET_CORE = "):]
	lineEndIdx := strings.Index(stmt, "\n")
	if lineEndIdx == -1 {
		lineEndIdx = len(stmt)
	}
	jsonStr := strings.TrimSpace(stmt[:lineEndIdx])
	jsonStr = strings.TrimSuffix(jsonStr, ";")
	var decoded string
	if err := json.Unmarshal([]byte(jsonStr), &decoded); err != nil {
		t.Fatalf("injected snippet literal is not valid JSON/JS string: %v\nJSON excerpt: %s", err, jsonStr[:min(100, len(jsonStr))])
	}
	if !strings.Contains(decoded, "__BASE__/api/cast?") {
		t.Fatal("decoded snippet does not contain __BASE__/api/cast?")
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
