package httpapi

import (
	"context"
	"errors"
	"io/fs"
	"net"
	"net/http"
	"path"
	"strings"
	"time"
)

var errBadUpstream = errors.New("upstream must be an absolute URL such as http://order-service:8081")

// dialer gives upstream connections a short timeout, so a service that is gone
// fails in seconds instead of hanging the page.
type dialer struct{ timeout time.Duration }

func (d *dialer) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	return (&net.Dialer{Timeout: d.timeout}).DialContext(ctx, network, addr)
}

// app serves the single-page front end: a file if the path names one, and
// index.html for every other path, so that a reload on /orders/ord_123 works.
func (s *Server) app(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
	if name != "" && name != "index.html" {
		if info, err := fs.Stat(s.ui, name); err == nil && !info.IsDir() {
			if strings.HasPrefix(name, "assets/") {
				// Vite puts a content hash in these file names, so they never change.
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			}
			http.ServeFileFS(w, r, s.ui, name)
			return
		}
		if path.Ext(name) != "" { // a missing .js or .png is a 404, not the app
			http.NotFound(w, r)
			return
		}
	}
	index, err := fs.ReadFile(s.ui, "index.html")
	if err != nil {
		http.Error(w, "The front end has not been built. Run `npm run build` in web-service/frontend, or use the Docker image.", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(index)
}
