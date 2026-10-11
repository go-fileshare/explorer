// SPDX-License-Identifier: BSD-3-Clause

//go:build !(js && wasm)

// The native build. Browsing files natively without a portal is the next
// step; -demo opens the explorer over a built-in stand-in, with real WebDAV
// over a temporary directory, to see it and try it.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/go-widgets/mvvm"
	"github.com/go-widgets/window"

	"github.com/go-fileshare/portal/client"

	"github.com/go-fileshare/explorer/internal/demo"
)

func main() {
	demoFlag := flag.Bool("demo", false, "run over a built-in stand-in: one server, real WebDAV over a temporary directory")
	flag.Parse()
	if !*demoFlag {
		fmt.Fprintln(os.Stderr, "explorer: the native explorer cannot log in on its own yet; run it in a browser, served by go-fileshare/portal, or try it with -demo")
		os.Exit(2)
	}
	dir, err := os.MkdirTemp("", "explorer-demo")
	if err != nil {
		fmt.Fprintln(os.Stderr, "explorer:", err)
		os.Exit(1)
	}
	defer os.RemoveAll(dir)
	d, err := demo.New(dir, true)
	if err != nil {
		fmt.Fprintln(os.Stderr, "explorer:", err)
		os.Exit(1)
	}
	defer d.Close()
	p := client.New(d.URL, nil, nil)

	w, err := window.Open(window.Config{Title: "fileshare explorer (demo)", Width: 1000, Height: 640})
	if err != nil {
		fmt.Fprintln(os.Stderr, "explorer:", err)
		os.Exit(1)
	}
	var wake func()
	if r, ok := w.(window.Repainter); ok {
		wake = r.Repaint
	}
	q := mvvm.NewQueue(wake)
	h := &nativeHost{p: p, dir: downloadsDir()}
	vm := newViewModel(context.Background(), p, h, q)
	h.status = func(s string) { q.Post(func() { vm.Status.Set(s) }) }
	v := newView(vm, q, nil)
	vm.Start()
	if err := w.Run(v.root); err != nil {
		fmt.Fprintln(os.Stderr, "explorer:", err)
		os.Exit(1)
	}
}

// nativeHost downloads into a folder, streaming, and has no file picker yet.
type nativeHost struct {
	p      *client.Portal
	dir    string
	status func(string)
}

func downloadsDir() string {
	if h, err := os.UserHomeDir(); err == nil {
		if fi, err := os.Stat(filepath.Join(h, "Downloads")); err == nil && fi.IsDir() {
			return filepath.Join(h, "Downloads")
		}
	}
	return os.TempDir()
}

func (h *nativeHost) Navigate(string) {}

func (h *nativeHost) PickFiles(func([]upload)) {
	h.status("Uploading from the native explorer needs a file picker, which is not there yet: use the browser.")
}

// Download writes the file at u into the downloads folder as it arrives,
// never whole in memory, refusing to replace a file already there.
func (h *nativeHost) Download(u string) {
	go func() {
		h.status(h.download(u))
	}()
}

func (h *nativeHost) download(u string) string {
	pu, err := url.Parse(u)
	if err != nil {
		return "Download: " + err.Error()
	}
	name := path.Base(pu.Path)
	if strings.HasSuffix(pu.Path, "/") || name == "." || name == "/" || name == ".." { // a folder is no file
		return "Download: no file name in " + u
	}
	// The name is the server's (an href it listed): it must name a file in
	// the folder, on every system -- a backslash is a separator on Windows,
	// which path.Base does not know.
	if strings.ContainsAny(name, `/\:`) || !filepath.IsLocal(name) {
		return fmt.Sprintf("Download: %q is no name to save a file under", name)
	}
	server := davServer(pu.Path)
	req, _ := http.NewRequest(http.MethodGet, u, nil)
	res, err := h.p.Doer(server, 0).Do(req)
	if err != nil {
		return "Download: " + err.Error()
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Sprintf("Download: the server answered %d", res.StatusCode)
	}
	dst := filepath.Join(h.dir, name)
	f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return "Download: " + err.Error()
	}
	n, err := io.Copy(f, res.Body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(dst)
		return "Download: " + err.Error()
	}
	return fmt.Sprintf("Downloaded %s (%s) to %s.", name, bytesString(n), h.dir)
}

// davServer is the server a /dav/<server>/... path is for.
func davServer(p string) string {
	segs := strings.Split(strings.TrimPrefix(p, "/"), "/")
	if len(segs) > 1 && segs[0] == "dav" {
		return segs[1]
	}
	return ""
}
