package adminui

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed static/index.html
var indexHTML []byte

//go:embed static
var staticFS embed.FS

// ServeShell writes the SPA shell (index.html).
func ServeShell(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(indexHTML)
}

// Handler serves the embedded static assets (css/js).
func Handler() http.Handler {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		panic("adminui embed: " + err.Error())
	}
	return http.FileServer(http.FS(sub))
}
