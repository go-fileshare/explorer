// SPDX-License-Identifier: BSD-3-Clause

//go:build !(js && wasm)

package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-fileshare/portal/client"

	"github.com/go-fileshare/explorer/internal/dav"
	"github.com/go-fileshare/explorer/internal/demo"
	"github.com/go-fileshare/explorer/internal/direct"
)

// The native download streams the file into the folder, and never replaces
// one already there, nor writes outside the folder.
func TestNativeDownload(t *testing.T) {
	d, err := demo.New(t.TempDir(), true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.Close)
	out := t.TempDir()
	said := make(chan string, 1)
	h := &nativeHost{dir: out, status: func(s string) { said <- s }}
	c := portalBackend{client.New(d.URL, nil, nil)}.DAV("demo")
	readme := dav.Entry{Name: "readme.txt", Path: "/files/readme.txt"}
	h.Download(c, readme)
	if s := <-said; !strings.HasPrefix(s, "Downloaded readme.txt (29 B)") {
		t.Fatal(s)
	}
	if b, _ := os.ReadFile(filepath.Join(out, "readme.txt")); !strings.HasPrefix(string(b), "Files on") {
		t.Fatalf("%q", b)
	}
	_ = os.WriteFile(filepath.Join(out, "readme.txt"), []byte("mine"), 0o644)
	if s := h.download(c, readme); !strings.Contains(s, "exists") {
		t.Fatalf("a second download: %q", s)
	}
	if b, _ := os.ReadFile(filepath.Join(out, "readme.txt")); string(b) != "mine" {
		t.Fatal("a download replaced a file")
	}
	for _, e := range []dav.Entry{
		{Name: `..\..\evil`, Path: "/files/x"},
		{Name: "c:evil", Path: "/files/x"},
		{Name: "..", Path: "/files/x"},
		{Name: "photos", Path: "/files/photos/", Dir: true},
	} {
		if s := h.download(c, e); !strings.Contains(s, "no name to save") {
			t.Errorf("%q: %q", e.Name, s)
		}
	}
	if s := h.download(c, dav.Entry{Name: "none.txt", Path: "/files/none.txt"}); !strings.Contains(s, "404") {
		t.Errorf("a missing file: %q", s)
	}
	// Into a folder that is not there: said, and nothing half-written.
	h.dir = filepath.Join(out, "missing")
	if s := h.download(c, readme); !strings.HasPrefix(s, "Download: ") {
		t.Fatal(s)
	}
}

// Natively, "log in again" starts again, and a provider's page opens in the
// system browser (not exercised: it would open one).
func TestNativeNavigateAndPick(t *testing.T) {
	relogins := 0
	pk := newPicker(osFS{}, t.TempDir())
	h := &nativeHost{relogin: func() { relogins++ }, pick: pk}
	h.Navigate(direct.LoginAgain)
	h.Navigate("javascript:alert(1)") // not https: ignored
	if relogins != 1 {
		t.Fatal(relogins)
	}
	h.PickFiles(func([]upload) {})
	if !pk.Visible.Get() {
		t.Fatal("the picker did not open")
	}
}

func TestDownloadsDir(t *testing.T) {
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

// The disk as the picker sees it: files with their size, folders, and only
// regular files offered for upload, read a piece at a time.
func TestOSFS(t *testing.T) {
	dir := t.TempDir()
	_ = os.Mkdir(filepath.Join(dir, "sub"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, "a.txt"), []byte("0123456789"), 0o644)
	if err := os.Symlink(filepath.Join(dir, "nowhere"), filepath.Join(dir, "dangling")); err != nil {
		t.Log("no symlink here:", err)
	}
	es, err := osFS{}.List(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]localEntry{}
	for _, e := range es {
		got[e.Name] = e
	}
	if !got["sub"].Dir || got["a.txt"].Size != 10 || len(got) != 2 {
		t.Fatalf("%+v", es)
	}
	if _, err := (osFS{}).List(filepath.Join(dir, "none")); err == nil {
		t.Error("a folder that is not there")
	}
	u, err := osFS{}.Source(filepath.Join(dir, "a.txt"))
	if err != nil || u.Name != "a.txt" || u.Src.Size() != 10 {
		t.Fatalf("%+v %v", u, err)
	}
	if b, err := u.Src.ReadPiece(context.Background(), 3, 4); err != nil || string(b) != "3456" {
		t.Fatalf("%q %v", b, err)
	}
	// Shrunk while being sent: an error, not a short piece.
	_ = os.WriteFile(filepath.Join(dir, "a.txt"), []byte("01"), 0o644)
	if _, err := u.Src.ReadPiece(context.Background(), 3, 4); err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("a shrunk file: %v", err)
	}
	_ = os.Remove(filepath.Join(dir, "a.txt"))
	if _, err := u.Src.ReadPiece(context.Background(), 0, 1); err == nil {
		t.Error("a removed file")
	}
	for _, p := range []string{filepath.Join(dir, "sub"), filepath.Join(dir, "none")} {
		if _, err := (osFS{}).Source(p); err == nil {
			t.Errorf("%s offered for upload", p)
		}
	}
}
