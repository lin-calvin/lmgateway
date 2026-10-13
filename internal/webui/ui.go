// Package webui serves the embedded gateway admin console. The console is a
// static, dependency-free page (vanilla ES modules + native CSS) that talks to
// the same JSON management API as lmgcli. Its operation logic mirrors
// internal/lmgcli/client.go one-to-one so the UI and the CLI never diverge.
package webui

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed static
var files embed.FS

// Handler serves the console assets. Mount it at /ui/ with http.StripPrefix.
func Handler() http.Handler {
	root, _ := fs.Sub(files, "static")
	fileServer := http.FileServer(http.FS(root))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Assets ship inside the binary and change with it; avoid stale caches.
		w.Header().Set("Cache-Control", "no-store")
		fileServer.ServeHTTP(w, r)
	})
}
