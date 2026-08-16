// Package web embeds the public web assets and the browser extraction
// snippet. Keeping them in a top-level directory separates frontend content
// from Go code while still allowing compile-time embedding.
package web

import "embed"

//go:embed index.html app.js style.css cast.js
var content embed.FS

// Asset returns the embedded asset with the given name (e.g. "index.html").
// cast.test.js is intentionally not embedded - it is a Node unit test, not
// served content.
func Asset(name string) ([]byte, error) {
	return content.ReadFile(name)
}
