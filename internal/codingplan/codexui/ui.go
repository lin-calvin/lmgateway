package codexui

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed static/index.html
var files embed.FS

func Handler() http.Handler {
	root, _ := fs.Sub(files, "static")
	return http.FileServer(http.FS(root))
}
