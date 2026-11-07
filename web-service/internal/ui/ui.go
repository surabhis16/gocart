// Package ui embeds the built front end. The files in dist/ are produced by
// `npm run build` in ../../frontend (the Dockerfile does this before compiling
// the Go code); a checkout that has not built the front end still compiles and
// serves a page saying so.
package ui

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var embedded embed.FS

// Files returns the built front end, rooted at its index.html.
func Files() fs.FS {
	sub, err := fs.Sub(embedded, "dist")
	if err != nil {
		panic(err) // the directory is embedded above; it cannot be missing
	}
	return sub
}
