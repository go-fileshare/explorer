// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-widgets/mvvm"

	"github.com/go-fileshare/portal/client"

	"github.com/go-fileshare/explorer/internal/dav"
	"github.com/go-fileshare/explorer/internal/demo"
)

// fakeHost records what the explorer asked of the page.
type fakeHost struct {
	mu        sync.Mutex
	downloads []string
	navigated []string
	picks     []upload
	delivered chan struct{} // a pick handed over: the test waits for it
}

func (h *fakeHost) Download(c *dav.Client, e dav.Entry) {
	h.mu.Lock()
	h.downloads = append(h.downloads, c.DownloadURL(e.Path))
	h.mu.Unlock()
}
func (h *fakeHost) Navigate(u string) {
	h.mu.Lock()
	h.navigated = append(h.navigated, u)
	h.mu.Unlock()
}
func (h *fakeHost) PickFiles(got func([]upload)) {
	h.mu.Lock()
	ups := h.picks
	h.mu.Unlock()
	go func() { // as a browser callback would: another goroutine
		got(ups)
		h.delivered <- struct{}{}
	}()
}

type bytesSource []byte

func (b bytesSource) Size() int64 { return int64(len(b)) }
func (b bytesSource) ReadPiece(_ context.Context, off, n int64) ([]byte, error) {
	return b[off : off+n], nil
}

type fixture struct {
	t  *testing.T
	d  *demo.Portal
	h  *fakeHost
	vm *viewModel
	q  *mvvm.Queue
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	d, err := demo.New(t.TempDir(), true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.Close)
	f := &fixture{t: t, d: d, h: &fakeHost{delivered: make(chan struct{}, 1)}, q: mvvm.NewQueue(nil)}
	f.vm = newViewModel(context.Background(), portalBackend{client.New(d.URL, nil, nil)}, f.h, f.q)
	f.vm.Start()
	f.settle()
	return f
}

func (f *fixture) settle() {
	f.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for f.vm.pending > 0 || f.q.Len() > 0 {
		f.q.Drain()
		if time.Now().After(deadline) {
			f.t.Fatal("still busy after 10 s")
		}
		time.Sleep(time.Millisecond)
	}
}

func (f *fixture) names() string {
	var out []string
	for _, e := range f.vm.Entries.Slice() {
		out = append(out, e.Name)
	}
	return strings.Join(out, ",")
}

// sel selects the entry called name.
func (f *fixture) sel(name string) {
	f.t.Helper()
	for i, e := range f.vm.Entries.Slice() {
		if e.Name == name {
			f.vm.EntrySel.Set(i)
			return
		}
	}
	f.t.Fatalf("no %q in %s", name, f.names())
}

func TestBrowse(t *testing.T) {
	f := newFixture(t)
	vm := f.vm
	if vm.User.Get() != "demo" || vm.Servers.Len() != 1 || vm.Dir.Get() != "/" || f.names() != "files" {
		t.Fatalf("user %q, dir %q, entries %s", vm.User.Get(), vm.Dir.Get(), f.names())
	}
	// At the root -- the shares -- nothing can be made or deleted.
	vm.Name.Set("x")
	if vm.NewFolder.CanExecute() || vm.Delete.CanExecute() || vm.Upload.CanExecute() || vm.Up.CanExecute() {
		t.Fatal("commands offered among the shares")
	}
	vm.Open.Execute()
	f.settle()
	if vm.Dir.Get() != "/files/" || f.names() != "photos,notes.md,readme.txt" {
		t.Fatalf("%q: %s", vm.Dir.Get(), f.names())
	}
	if line := entryLine(vm.Entries.At(2)); !strings.HasPrefix(line, "readme.txt  29 B  ") {
		t.Fatalf("line %q", line)
	}
	f.sel("photos")
	vm.Open.Execute()
	f.settle()
	vm.Open.Execute() // 2026
	f.settle()
	if vm.Dir.Get() != "/files/photos/2026/" || f.names() != "summer.jpg" {
		t.Fatal(vm.Dir.Get(), f.names())
	}
	// Up keeps the folder we came from selected.
	vm.Up.Execute()
	f.settle()
	if e, _ := vm.entry(); vm.Dir.Get() != "/files/photos/" || e.Name != "2026" {
		t.Fatalf("%q, selected %q", vm.Dir.Get(), e.Name)
	}
	vm.Up.Execute()
	f.settle()
	vm.Up.Execute()
	f.settle()
	if vm.Dir.Get() != "/" {
		t.Fatal(vm.Dir.Get())
	}
}

func (f *fixture) into(dir string) {
	f.t.Helper()
	f.vm.Dir.Set(dir)
	f.vm.Reload.Execute()
	f.settle()
}

func TestFolderRenameDuplicateDelete(t *testing.T) {
	f := newFixture(t)
	vm := f.vm
	f.into("/files/")
	vm.Name.Set("drafts")
	vm.NewFolder.Execute()
	f.settle()
	if e, _ := vm.entry(); e.Name != "drafts" || !e.Dir {
		t.Fatalf("selected %+v, %s", e, f.names())
	}
	// Rename notes.md, then refuse a rename onto readme.txt.
	f.sel("notes.md")
	vm.Name.Set("ideas.md")
	vm.Rename.Execute()
	f.settle()
	if !strings.Contains(f.names(), "ideas.md") || strings.Contains(f.names(), "notes.md") {
		t.Fatal(f.names())
	}
	f.sel("ideas.md")
	vm.Name.Set("readme.txt")
	vm.Rename.Execute()
	f.settle()
	if !strings.Contains(vm.Status.Get(), "there already") {
		t.Fatal(vm.Status.Get())
	}
	if b, _ := os.ReadFile(filepath.Join(f.d.Dir, "readme.txt")); !strings.HasPrefix(string(b), "Files on") {
		t.Fatal("a refused rename replaced the file")
	}
	// Duplicate a folder, then delete the copy in two presses.
	f.sel("photos")
	vm.Name.Set("photos-copy")
	vm.Duplicate.Execute()
	f.settle()
	if _, err := os.Stat(filepath.Join(f.d.Dir, "photos-copy", "2026", "summer.jpg")); err != nil {
		t.Fatal(err)
	}
	f.sel("photos-copy")
	vm.Delete.Execute()
	if !strings.Contains(vm.Status.Get(), "and everything in it") {
		t.Fatal(vm.Status.Get())
	}
	vm.Delete.Execute()
	f.settle()
	if _, err := os.Stat(filepath.Join(f.d.Dir, "photos-copy")); !os.IsNotExist(err) {
		t.Fatal("not deleted")
	}
	// Arming does not survive a change of selection.
	f.sel("drafts")
	vm.Delete.Execute()
	f.sel("photos")
	vm.Delete.Execute()
	f.settle()
	if _, err := os.Stat(filepath.Join(f.d.Dir, "photos")); err != nil {
		t.Fatal("a press armed for one entry deleted another")
	}
	// Names that are not names.
	for _, bad := range []string{"a/b", "..", "."} {
		vm.Name.Set(bad)
		vm.NewFolder.Execute()
		if !strings.Contains(vm.Status.Get(), "is not a name") {
			t.Errorf("%q: %q", bad, vm.Status.Get())
		}
	}
	vm.Name.Set("photos")
	f.sel("photos")
	vm.Rename.Execute()
	if !strings.Contains(vm.Status.Get(), "another name first") {
		t.Fatal(vm.Status.Get())
	}
}

func TestUploadAndDownload(t *testing.T) {
	f := newFixture(t)
	vm := f.vm
	f.into("/files/")
	big := bytes.Repeat([]byte("explorer"), 3<<20) // 24 MiB: three pieces of 8
	f.h.picks = []upload{{Name: "big.bin", Src: bytesSource(big)}, {Name: "small.txt", Src: bytesSource("hi")}}
	vm.Upload.Execute()
	<-f.h.delivered
	f.settle()
	got, err := os.ReadFile(filepath.Join(f.d.Dir, "big.bin"))
	if err != nil || !bytes.Equal(got, big) {
		t.Fatalf("%d bytes, %v; status %q", len(got), err, vm.Status.Get())
	}
	if !strings.HasPrefix(vm.Status.Get(), "2 uploaded") || !strings.Contains(f.names(), "small.txt") {
		t.Fatalf("%q, %s", vm.Status.Get(), f.names())
	}
	// Uploading over an existing name is refused, never a replacement.
	f.h.picks = []upload{{Name: "small.txt", Src: bytesSource("REPLACED")}}
	vm.Upload.Execute()
	<-f.h.delivered
	f.settle()
	if b, _ := os.ReadFile(filepath.Join(f.d.Dir, "small.txt")); string(b) != "hi" || !strings.Contains(vm.Status.Get(), "there already") {
		t.Fatalf("%q, %q", b, vm.Status.Get())
	}
	// Download: the token first, then a navigation the page follows.
	f.sel("big.bin")
	vm.Download.Execute()
	f.settle()
	if len(f.h.downloads) != 1 || f.h.downloads[0] != f.d.URL+"/dav/demo/files/big.bin" {
		t.Fatalf("%v", f.h.downloads)
	}
	// Open on a file downloads it.
	f.sel("small.txt")
	vm.Open.Execute()
	f.settle()
	if len(f.h.downloads) != 2 {
		t.Fatal(f.h.downloads)
	}
	// Nothing picked: nothing happens.
	f.h.picks = nil
	vm.Upload.Execute()
	<-f.h.delivered
	f.settle()
}

func TestLogoutAndEndedSession(t *testing.T) {
	f := newFixture(t)
	f.vm.Logout.Execute()
	f.settle()
	if len(f.h.navigated) != 1 || !strings.Contains(f.h.navigated[0], "/login?return=%2Fexplorer%2F") {
		t.Fatal(f.h.navigated)
	}
	// The stand-in dies: the explorer says so, rather than hanging.
	f.d.Close()
	f.vm.Reload.Execute()
	f.settle()
	if !strings.HasPrefix(f.vm.Status.Get(), "List /: ") {
		t.Fatal(f.vm.Status.Get())
	}
}

// What a failure says: in words, for each way a server refuses, and a login
// that has ended sends the browser to log in again.
func TestFailures(t *testing.T) {
	f := newFixture(t)
	for err, want := range map[error]string{
		&dav.Error{Status: 401}: "x: the server refused this login",
		&dav.Error{Status: 403}: "x: not allowed",
		&dav.Error{Status: 404}: "x: not found",
		&dav.Error{Status: 507}: "x: the share is full",
		&dav.Error{Status: 500}: "x: the server answered 500",
		&dav.Error{Status: 412}: "x: something with that name is there already",
		errors.New("broken"):    "x: broken",
	} {
		f.vm.fail("x", err)
		if got := f.vm.Status.Get(); got != want {
			t.Errorf("%v: %q, want %q", err, got, want)
		}
	}
	// An upload that can be resumed says how; a refused one does not.
	f.vm.fail("Upload", errors.New("connection reset"))
	if !strings.HasSuffix(f.vm.Status.Get(), "to resume where it stopped.") {
		t.Errorf("a resumable upload: %q", f.vm.Status.Get())
	}
	f.vm.fail("Upload", &dav.Error{Status: 507})
	if strings.Contains(f.vm.Status.Get(), "resume") {
		t.Errorf("a refused upload offered a resume: %q", f.vm.Status.Get())
	}
	f.vm.fail("x", fmt.Errorf("wrapped: %w", client.ErrNotLoggedIn))
	if len(f.h.navigated) != 1 || !strings.Contains(f.vm.Status.Get(), "not logged in") {
		t.Fatal(f.h.navigated, f.vm.Status.Get())
	}
}

// A login that reaches no server's files says so, rather than an empty
// window.
func TestNoServer(t *testing.T) {
	// One server whose files the portal does not pass, one this login
	// cannot reach: neither is listed.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"subject":"s","servers":[{"id":"a","reachable":true},{"id":"b","dav":true}]}`))
	}))
	t.Cleanup(srv.Close)
	q := mvvm.NewQueue(nil)
	vm := newViewModel(context.Background(), portalBackend{client.New(srv.URL, nil, nil)}, &fakeHost{}, q)
	vm.Start()
	for vm.pending > 0 || q.Len() > 0 {
		q.Drain()
		time.Sleep(time.Millisecond)
	}
	if vm.Servers.Len() != 0 || !strings.Contains(vm.Status.Get(), "No server") {
		t.Fatalf("%d servers, %q", vm.Servers.Len(), vm.Status.Get())
	}
	if vm.Open.CanExecute() || vm.Reload.CanExecute() {
		t.Fatal("commands offered with no server")
	}
}

func TestBytesString(t *testing.T) {
	for n, want := range map[int64]string{0: "0 B", 1023: "1023 B", 1536: "1.5 KiB", 3 << 30: "3.0 GiB"} {
		if got := bytesString(n); got != want {
			t.Errorf("%d: %s", n, got)
		}
	}
}
