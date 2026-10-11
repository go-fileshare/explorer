// SPDX-License-Identifier: BSD-3-Clause

// Package demo is a stand-in for go-fileshare/portal with one server, whose
// files are the real WebDAV server fileshare serves shares with
// (go-filesystems/webdav) over a directory: what `explorer -demo` and the
// tests run against. It requires the portal's static header, as the portal
// does.
package demo

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/go-filesystems/osfs"
	"github.com/go-filesystems/webdav"
)

// Portal is the stand-in, serving dir as server "demo"'s share "files".
type Portal struct {
	*httptest.Server
	Dir  string
	fsys *osfs.FS
}

// Close stops the server, then lets go of the directory: osfs holds it open
// (an os.Root), and Windows will not remove a directory that is held.
func (p *Portal) Close() {
	p.Server.Close()
	_ = p.fsys.Close()
}

// New serves dir; with seed, a few files and a folder are put in it first.
func New(dir string, seed bool) (*Portal, error) {
	if seed {
		for name, body := range map[string]string{"readme.txt": "Files on a fileshare server.\n",
			"photos/2026/summer.jpg": "not really a picture", "notes.md": "# Notes\n"} {
			p := filepath.Join(dir, filepath.FromSlash(name))
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				return nil, err
			}
			if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
				return nil, err
			}
		}
	}
	fsys, err := osfs.Open(dir)
	if err != nil {
		return nil, err
	}
	// As fileshare serves a share: at /<share>/ of its WebDAV. The portal
	// passes /dav/<server>/<path> to it as /<path>, and does not rewrite what
	// comes back: hrefs are the server's own paths.
	share, err := webdav.New(fsys, webdav.WithPrefix("/files"), webdav.ReadWrite())
	if err != nil {
		_ = fsys.Close()
		return nil, err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /session", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"subject":"demo","name":"demo","servers":[{"id":"demo","label":"Demo server","reachable":true,"dav":true}]}`))
	})
	mux.HandleFunc("POST /session/refresh", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("{}")) })
	mux.HandleFunc("POST /logout", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("{}")) })
	// The root of the server's WebDAV lists its one share.
	mux.HandleFunc("/dav/demo/{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusMultiStatus)
		_, _ = w.Write([]byte(`<?xml version="1.0"?><D:multistatus xmlns:D="DAV:">` +
			`<D:response><D:href>/</D:href><D:propstat><D:prop><D:resourcetype><D:collection/></D:resourcetype></D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>` +
			`<D:response><D:href>/files/</D:href><D:propstat><D:prop><D:resourcetype><D:collection/></D:resourcetype></D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>` +
			`</D:multistatus>`))
	})
	mux.Handle("/dav/demo/files/", http.StripPrefix("/dav/demo", destination(share)))
	guarded := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		safe := r.Method == http.MethodGet || r.Method == http.MethodHead
		if !safe && r.Header.Get("Portal-Request") != "1" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		mux.ServeHTTP(w, r)
	})
	return &Portal{Server: httptest.NewServer(guarded), Dir: dir, fsys: fsys}, nil
}

// destination rewrites a MOVE's or COPY's Destination from the portal's URL
// to the server's path, as the portal does.
func destination(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if d := r.Header.Get("Destination"); d != "" {
			u, err := url.Parse(d)
			if err != nil || !strings.HasPrefix(u.Path, "/dav/demo/") {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			r.Header.Set("Destination", strings.TrimPrefix(u.Path, "/dav/demo"))
		}
		h.ServeHTTP(w, r)
	})
}
