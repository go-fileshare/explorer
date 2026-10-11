// SPDX-License-Identifier: BSD-3-Clause

//go:build !(js && wasm)

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-fileshare/portal/client"

	"github.com/go-fileshare/explorer/internal/demo"
)

// The native download streams the file into the folder, and never replaces
// one already there.
func TestNativeDownload(t *testing.T) {
	d, err := demo.New(t.TempDir(), true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.Close)
	out := t.TempDir()
	said := make(chan string, 1)
	h := &nativeHost{p: client.New(d.URL, nil, nil), dir: out, status: func(s string) { said <- s }}
	u := d.URL + "/dav/demo/files/readme.txt"
	h.Download(u)
	if s := <-said; !strings.HasPrefix(s, "Downloaded readme.txt (29 B)") {
		t.Fatal(s)
	}
	if b, _ := os.ReadFile(filepath.Join(out, "readme.txt")); !strings.HasPrefix(string(b), "Files on") {
		t.Fatalf("%q", b)
	}
	_ = os.WriteFile(filepath.Join(out, "readme.txt"), []byte("mine"), 0o644)
	if s := h.download(u); !strings.Contains(s, "exists") {
		t.Fatalf("a second download: %q", s)
	}
	if b, _ := os.ReadFile(filepath.Join(out, "readme.txt")); string(b) != "mine" {
		t.Fatal("a download replaced a file")
	}
	for in, want := range map[string]string{
		d.URL + "/dav/demo/files/none.txt":       "answered 404",
		d.URL + "/dav/demo/":                     "no file name",
		"::":                                     "Download: ",
		d.URL + `/dav/demo/files/..%5C..%5Cevil`: "no name to save",
		d.URL + `/dav/demo/files/c:evil`:         "no name to save",
		"http://127.0.0.1:1/dav/a/f":             "Download: ",
	} {
		if s := h.download(in); !strings.Contains(s, want) {
			t.Errorf("%s: %q", in, s)
		}
	}
	// Into a folder that is not there: said, and nothing half-written.
	h.dir = filepath.Join(out, "missing")
	if s := h.download(u); !strings.HasPrefix(s, "Download: ") {
		t.Fatal(s)
	}
	h.PickFiles(nil)
	if s := <-said; !strings.Contains(s, "file picker") {
		t.Fatal(s)
	}
	h.Navigate("x")
}

func TestDavServerAndDownloadsDir(t *testing.T) {
	for in, want := range map[string]string{"/dav/a/f": "a", "/dav/b/": "b", "/api/a/x": "", "/dav": ""} {
		if got := davServer(in); got != want {
			t.Errorf("%s: %q", in, got)
		}
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	if got := downloadsDir(); got != os.TempDir() {
		t.Errorf("no Downloads: %s", got)
	}
	_ = os.Mkdir(filepath.Join(home, "Downloads"), 0o755)
	if got := downloadsDir(); got != filepath.Join(home, "Downloads") {
		t.Errorf("%s", got)
	}
}
