package proxy

import "testing"

func TestSafeLogURL(t *testing.T) {
	got := SafeLogURL("https://user:password@example.com/video?token=secret#fragment")
	if got != "https://example.com/video" {
		t.Fatal(got)
	}
}
