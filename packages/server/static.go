package server

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strings"
)

// contentSecurityPolicy only lets the page load its own scripts, styles and
// API: task text is always inserted as text, and this is the second line of
// defence should that ever slip.
const contentSecurityPolicy = "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; " +
	"connect-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'; object-src 'none'"

type staticFile struct {
	body  []byte
	ctype string
	etag  string
}

// loadStatic reads every file of the web UI into memory once. A missing
// index.html yields an empty site and the built-in fallback page.
func loadStatic(assets fs.FS) (map[string]staticFile, error) {
	files := map[string]staticFile{}
	if assets == nil {
		return files, nil
	}
	err := fs.WalkDir(assets, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || strings.HasPrefix(d.Name(), ".") || strings.HasSuffix(p, ".go") {
			return err
		}
		b, err := fs.ReadFile(assets, p)
		if err != nil {
			return err
		}
		ctype := mime.TypeByExtension(path.Ext(p))
		switch path.Ext(p) {
		case ".js", ".mjs":
			ctype = "text/javascript; charset=utf-8"
		case ".html":
			ctype = "text/html; charset=utf-8"
		case ".css":
			ctype = "text/css; charset=utf-8"
		}
		if ctype == "" {
			ctype = "application/octet-stream"
		}
		sum := sha256.Sum256(b)
		files[p] = staticFile{body: b, ctype: ctype, etag: `"` + hex.EncodeToString(sum[:8]) + `"`}
		return nil
	})
	if errors.Is(err, fs.ErrNotExist) {
		return files, nil
	}
	return files, err
}

// handleStatic serves the web UI: "/" is index.html, other paths map to
// asset files. Assets are revalidated on every load (ETag) so an upgraded
// binary never runs against stale scripts.
func (s *Server) handleStatic(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "405 method not allowed", http.StatusMethodNotAllowed)
		return
	}
	name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
	if name == "" {
		name = "index.html"
	}
	f, ok := s.static[name]
	if !ok {
		if name == "index.html" {
			s.handleFallbackIndex(w, r)
			return
		}
		http.Error(w, "404 page not found", http.StatusNotFound)
		return
	}
	h := w.Header()
	h.Set("Content-Type", f.ctype)
	h.Set("ETag", f.etag)
	h.Set("Cache-Control", "no-cache")
	if strings.HasPrefix(f.ctype, "text/html") {
		h.Set("Content-Security-Policy", contentSecurityPolicy)
		h.Set("X-Frame-Options", "DENY")
	}
	if match := r.Header.Get("If-None-Match"); match != "" && match == f.etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(f.body)
	}
}
