// SPDX-License-Identifier: BSD-3-Clause

//go:build !(js && wasm)

// The native build. It logs in itself -- the system browser and a loopback
// listener -- and calls each server's WebDAV with a token for that server
// (internal/direct), the servers read from explorer.hcl. -demo opens it over
// a built-in stand-in instead, with real WebDAV over a temporary directory.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/go-authn/rp"
	"github.com/go-widgets/mvvm"
	"github.com/go-widgets/window"

	"github.com/go-fileshare/portal/client"

	"github.com/go-fileshare/explorer/internal/dav"
	"github.com/go-fileshare/explorer/internal/demo"
	"github.com/go-fileshare/explorer/internal/direct"
)

func main() {
	demoFlag := flag.Bool("demo", false, "run over a built-in stand-in: one server, real WebDAV over a temporary directory")
	config := flag.String("config", defaultConfigPath(), "the servers, and where to log in")
	flag.Parse()
	var be backend
	if *demoFlag {
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
		be = portalBackend{client.New(d.URL, nil, nil)}
	} else {
		cfg, err := loadNative(*config)
		if err != nil {
			fmt.Fprintf(os.Stderr, "explorer: %v\n(the servers and the identity provider are read from %s; -demo runs without them)\n", err, *config)
			os.Exit(2)
		}
		d, err := direct.New(cfg, rp.OpenBrowser, nil)
		if err != nil {
			fmt.Fprintln(os.Stderr, "explorer:", err)
			os.Exit(2)
		}
		be = d
	}

	w, err := window.Open(window.Config{Title: "fileshare explorer", Width: 1000, Height: 640})
	if err != nil {
		fmt.Fprintln(os.Stderr, "explorer:", err)
		os.Exit(1)
	}
	var wake func()
	if r, ok := w.(window.Repainter); ok {
		wake = r.Repaint
	}
	q := mvvm.NewQueue(wake)
	home, _ := os.UserHomeDir()
	pk := newPicker(osFS{}, home)
	h := &nativeHost{dir: downloadsDir(), pick: pk}
	vm := newViewModel(context.Background(), be, h, q)
	h.status = func(s string) { q.Post(func() { vm.Status.Set(s) }) }
	h.relogin = func() { vm.Status.Set("Log in in your browser..."); vm.Start() }
	v := newView(vm, pk, q, nil)
	if !*demoFlag {
		vm.Status.Set("Log in in your browser...")
	}
	vm.Start()
	if err := w.Run(v.root); err != nil {
		fmt.Fprintln(os.Stderr, "explorer:", err)
		os.Exit(1)
	}
}

// nativeHost downloads into a folder, streaming, and picks files with the
// explorer's own picker.
type nativeHost struct {
	dir     string
	pick    *picker
	status  func(string) // from any goroutine
	relogin func()       // on the UI goroutine
}

func downloadsDir() string {
	if h, err := os.UserHomeDir(); err == nil {
		if fi, err := os.Stat(filepath.Join(h, "Downloads")); err == nil && fi.IsDir() {
			return filepath.Join(h, "Downloads")
		}
	}
	return os.TempDir()
}

// Navigate is where a browser page would go: natively, logging in again is
// starting again -- the backend opens the system browser -- and a provider's
// end-of-session page opens in the system browser.
func (h *nativeHost) Navigate(to string) {
	switch {
	case to == direct.LoginAgain:
		if h.relogin != nil {
			h.relogin()
		}
	case strings.HasPrefix(to, "https://"):
		_ = rp.OpenBrowser(to)
	}
}

func (h *nativeHost) PickFiles(got func([]upload)) { h.pick.Show(got) }

// Download writes e into the downloads folder as it arrives, never whole in
// memory, refusing to replace a file already there.
func (h *nativeHost) Download(c *dav.Client, e dav.Entry) {
	go func() { h.status(h.download(c, e)) }()
}

func (h *nativeHost) download(c *dav.Client, e dav.Entry) string {
	// The name is the server's (an href it listed): it must name a file in
	// the folder, on every system -- a backslash is a separator on Windows,
	// and a drive letter would leave the folder.
	name := e.Name
	if e.Dir || strings.ContainsAny(name, `/\:`) || !filepath.IsLocal(name) {
		return fmt.Sprintf("Download: %q is no name to save a file under", name)
	}
	rc, err := c.Get(context.Background(), e.Path)
	if err != nil {
		return "Download: " + err.Error()
	}
	defer rc.Close()
	dst := filepath.Join(h.dir, name)
	f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return "Download: " + err.Error()
	}
	n, err := io.Copy(f, rc)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(dst)
		return "Download: " + err.Error()
	}
	return fmt.Sprintf("Downloaded %s (%s) to %s.", name, bytesString(n), h.dir)
}

// osFS is this computer's disk, for the picker.
type osFS struct{}

func (osFS) List(dir string) ([]localEntry, error) {
	des, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	out := make([]localEntry, 0, len(des))
	for _, de := range des {
		e := localEntry{Name: de.Name(), Dir: de.IsDir()}
		if !e.Dir {
			fi, err := de.Info()
			if err != nil || !fi.Mode().IsRegular() {
				continue // a device, a socket, a link to nowhere: nothing to upload
			}
			e.Size = fi.Size()
		}
		out = append(out, e)
	}
	return out, nil
}

func (osFS) Source(path string) (upload, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return upload{}, err
	}
	if !fi.Mode().IsRegular() {
		return upload{}, fmt.Errorf("%s is not a file", path)
	}
	return upload{Name: filepath.Base(path), Src: localFile{path: path, size: fi.Size(), modified: fi.ModTime().UnixMilli()}}, nil
}

// localFile is a file of this computer, read a piece at a time and opened
// for each piece: nothing stays open once the upload is over, however it
// ends. A file that changes size meanwhile fails the upload.
type localFile struct {
	path     string
	size     int64
	modified int64 // ms since the epoch, as a browser's File says it
}

func (l localFile) Size() int64 { return l.size }

// Fingerprint is the file's name, size and time: an upload that failed is
// resumed by the same file only.
func (l localFile) Fingerprint() string {
	return dav.Fingerprint(filepath.Base(l.path), l.size, l.modified)
}

func (l localFile) ReadPiece(_ context.Context, off, n int64) ([]byte, error) {
	f, err := os.Open(l.path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b := make([]byte, n)
	if _, err := io.ReadFull(io.NewSectionReader(f, off, n), b); err != nil {
		return nil, fmt.Errorf("%s changed while it was being sent: %w", l.path, err)
	}
	return b, nil
}
