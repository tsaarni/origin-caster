package server

import (
	"sync"

	"github.com/tdewolff/minify/v2"
	"github.com/tdewolff/minify/v2/js"

	"github.com/tsaarni/origin-caster/web"
)

// minifiedSnippet is the single-line browser snippet, generated once at
// startup by minifying web/cast.js (the readable, modular detector source). The
// dashboard injects it into web/app.js when serving it.
var (
	snippetOnce sync.Once
	snippetCore string
	snippetErr  error
)

func minifiedSnippet() (string, error) {
	snippetOnce.Do(func() {
		castScript, err := web.Asset("cast.js")
		if err != nil {
			snippetErr = err
			return
		}
		m := minify.New()
		m.AddFunc("application/javascript", js.Minify)
		snippetCore, snippetErr = m.String("application/javascript", string(castScript))
	})
	return snippetCore, snippetErr
}
