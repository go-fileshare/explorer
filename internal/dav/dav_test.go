// SPDX-License-Identifier: BSD-3-Clause

package dav

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/go-filesystems/osfs"
	"github.com/go-filesystems/webdav"
)

// The client against the real WebDAV server fileshare serves its shares
// with (go-filesystems/webdav v0.6.0) over a real directory, with the
// share's prefix as fileshare sets it.
func serve(t *testing.T) (*Client, string, *atomic.Int64) {
	t.Helper()
	dir := t.TempDir()
	fsys, err := osfs.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	// After the server (cleanups run last first): the directory is held
	// open until then, and Windows cannot remove a held directory.
	t.Cleanup(func() { _ = fsys.Close() })
	h, err := webdav.New(fsys, webdav.WithPrefix("/files"), webdav.ReadWrite())
	if err != nil {
		t.Fatal(err)
	}
	var largest atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ContentLength > largest.Load() {
			largest.Store(r.ContentLength)
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return New(srv.URL, srv.Client()), dir, &largest
}

type bytesSource []byte

func (b bytesSource) Size() int64 { return int64(len(b)) }
func (b bytesSource) ReadPiece(_ context.Context, off, n int64) ([]byte, error) {
	return b[off : off+n], nil
}

func TestListMkdirMoveCopyDelete(t *testing.T) {
	c, dir, _ := serve(t)
	ctx := context.Background()
	_ = os.WriteFile(filepath.Join(dir, "b.txt"), []byte("bee"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "A.txt"), []byte("a"), 0o644)
	if err := c.Mkdir(ctx, "/files/zdir"); err != nil {
		t.Fatal(err)
	}
	if err := c.Mkdir(ctx, "/files/name with space é"); err != nil {
		t.Fatal(err)
	}
	es, err := c.List(ctx, "/files")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range es {
		names = append(names, e.Name)
	}
	if strings.Join(names, "|") != "name with space é|zdir|A.txt|b.txt" {
		t.Fatalf("%q", names)
	}
	if b := es[3]; b.Dir || b.Size != 3 || b.Path != "/files/b.txt" || b.Modified.IsZero() {
		t.Fatalf("%+v", b)
	}
	if !es[1].Dir || es[1].Path != "/files/zdir/" {
		t.Fatalf("%+v", es[1])
	}

	// Rename, refused onto an existing name, then copy, then delete.
	if err := c.Move(ctx, "/files/A.txt", "/files/b.txt"); !Exists(err) {
		t.Fatalf("a rename onto an existing file: %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "b.txt")); string(got) != "bee" {
		t.Fatal("a refused rename destroyed the file it landed on")
	}
	if err := c.Move(ctx, "/files/A.txt", "/files/zdir/a.txt"); err != nil {
		t.Fatal(err)
	}
	if err := c.Copy(ctx, "/files/zdir", "/files/zdir2"); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "zdir2", "a.txt")); string(got) != "a" {
		t.Fatal("the copy is not there")
	}
	if err := c.Delete(ctx, "/files/zdir2/"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "zdir2")); !os.IsNotExist(err) {
		t.Fatal("not deleted")
	}
	var e *Error
	if err := c.Delete(ctx, "/files/nothing"); !errors.As(err, &e) || e.Status != http.StatusNotFound || !strings.Contains(e.Error(), "delete /files/nothing: 404") {
		t.Fatalf("%v", err)
	}
	if _, err := c.List(ctx, "/files/nothing/"); err == nil {
		t.Fatal("a listing of nothing")
	}
}

// A file larger than a piece goes in pieces: no request carries it whole,
// nobody sees half of it under its name, and it arrives whole.
func TestUploadInPieces(t *testing.T) {
	c, dir, largest := serve(t)
	c.Piece = 100_000
	want := bytes.Repeat([]byte("0123456789"), 35_000) // 350 KB: four pieces
	var sent []int64
	if err := c.Upload(context.Background(), "/files/big.bin", bytesSource(want), false, func(n int64) { sent = append(sent, n) }); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "big.bin"))
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("%d bytes, %v", len(got), err)
	}
	if largest.Load() > c.Piece {
		t.Fatalf("a request carried %d bytes, more than a piece", largest.Load())
	}
	if len(sent) != 4 || sent[3] != int64(len(want)) {
		t.Fatalf("progress %v", sent)
	}
	if _, err := os.Stat(filepath.Join(dir, "big.bin.part")); !os.IsNotExist(err) {
		t.Fatal("the part file is left")
	}
	// Again, without overwrite: refused, and the part file cleaned up.
	if err := c.Upload(context.Background(), "/files/big.bin", bytesSource("short"), false, nil); !Exists(err) {
		t.Fatalf("an upload onto an existing file: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "big.bin.part")); !os.IsNotExist(err) {
		t.Fatal("a failed upload left its part file")
	}
	// With overwrite, it replaces. And an empty file is an upload too.
	if err := c.Upload(context.Background(), "/files/big.bin", bytesSource("short"), true, nil); err != nil {
		t.Fatal(err)
	}
	if err := c.Upload(context.Background(), "/files/empty", bytesSource(nil), false, nil); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(filepath.Join(dir, "empty")); err != nil || fi.Size() != 0 {
		t.Fatal(err)
	}
}

type failingSource struct{ bytesSource }

func (f failingSource) ReadPiece(ctx context.Context, off, n int64) ([]byte, error) {
	if off > 0 {
		return nil, errors.New("the file went away")
	}
	return f.bytesSource.ReadPiece(ctx, off, n)
}

type shortSource struct{ bytesSource }

func (s shortSource) ReadPiece(ctx context.Context, off, n int64) ([]byte, error) {
	return []byte("x"), nil
}

func TestUploadFailures(t *testing.T) {
	c, dir, _ := serve(t)
	c.Piece = 4
	for _, src := range []Source{failingSource{bytesSource("0123456789")}, shortSource{bytesSource("0123456789")}} {
		if err := c.Upload(context.Background(), "/files/f", src, false, nil); err == nil {
			t.Fatal("accepted")
		}
		if _, err := os.Stat(filepath.Join(dir, "f.part")); !os.IsNotExist(err) {
			t.Fatal("a failed upload left its part file")
		}
		if _, err := os.Stat(filepath.Join(dir, "f")); !os.IsNotExist(err) {
			t.Fatal("a failed upload left a file under the name")
		}
	}
	// A folder that is not there: the first PUT is refused.
	if err := c.Upload(context.Background(), "/files/no/f", bytesSource("x"), false, nil); err == nil {
		t.Fatal("an upload into nothing")
	}
}

func TestURLsAndUnreachable(t *testing.T) {
	c := New("https://portal.example/dav/a/", http.DefaultClient)
	if got := c.DownloadURL("/files/a b/é#?.txt"); got != "https://portal.example/dav/a/files/a%20b/%C3%A9%23%3F.txt" {
		t.Fatal(got)
	}
	dead := New("http://127.0.0.1:1", http.DefaultClient)
	ctx := context.Background()
	for name, err := range map[string]error{
		"list": func() error { _, err := dead.List(ctx, "/"); return err }(), "mkdir": dead.Mkdir(ctx, "/x"),
		"delete": dead.Delete(ctx, "/x"), "move": dead.Move(ctx, "/x", "/y"),
		"upload": dead.Upload(ctx, "/x", bytesSource("x"), false, nil),
	} {
		if err == nil {
			t.Errorf("%s with nothing listening", name)
		}
	}
	if (&Error{Op: "o", Path: "/p", Status: 507}).Error() != "o /p: 507 Insufficient Storage" {
		t.Error("Error()")
	}
}
