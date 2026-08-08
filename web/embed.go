// Package web serves the built panel from inside the binary.
//
// The whole point of the project's single-binary rule is that an operator
// copies one file onto a VPS and gets a working panel; shipping a dist/
// directory alongside it would break that.
package web

import (
	"embed"
	"errors"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// dist holds the production build. The all: prefix is required because Vite
// emits an assets/ directory and embed skips underscore- and dot-prefixed
// entries by default.
//
//go:embed all:dist
var dist embed.FS

// ErrNotBuilt reports that the panel has not been built yet, so the binary
// carries only a placeholder.
var ErrNotBuilt = errors.New("web: the panel has not been built (run npm run build in web/)")

// FS returns the built panel rooted at dist/.
func FS() (fs.FS, error) {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		return nil, err
	}
	if _, err := fs.Stat(sub, "index.html"); err != nil {
		return nil, ErrNotBuilt
	}
	return sub, nil
}

// Handler serves the panel, falling back to index.html for unknown paths.
//
// The fallback is what makes client-side routing work: a browser asked to
// open /servers/01ABC directly must still receive the app, which then reads
// the path itself. Requests under /assets are exempt, so a genuinely missing
// asset returns 404 rather than a page of HTML with a JavaScript content type.
func Handler() (http.Handler, error) {
	files, err := FS()
	if err != nil {
		return nil, err
	}

	index, err := fs.ReadFile(files, "index.html")
	if err != nil {
		return nil, err
	}

	fileServer := http.FileServer(http.FS(files))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")

		if name == "" || name == "." {
			serveIndex(w, index)
			return
		}

		if _, err := fs.Stat(files, name); err == nil {
			// Hashed asset filenames change on every build, so they can be
			// cached hard; index.html must not be, or a deploy would leave
			// browsers on the old app.
			if strings.HasPrefix(name, "assets/") {
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			}
			fileServer.ServeHTTP(w, r)
			return
		}

		if strings.HasPrefix(name, "assets/") {
			http.NotFound(w, r)
			return
		}

		serveIndex(w, index)
	}), nil
}

func serveIndex(w http.ResponseWriter, index []byte) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(index)
}
