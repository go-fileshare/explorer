// SPDX-License-Identifier: BSD-3-Clause

package dav

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// fpSource is a file that says which file it is, and can be made to fail
// when a piece at or after failAt is read.
type fpSource struct {
	bytesSource
	fp     string
	failAt int64
}

func (f fpSource) Fingerprint() string { return f.fp }
func (f fpSource) ReadPiece(ctx context.Context, off, n int64) ([]byte, error) {
	if f.failAt > 0 && off >= f.failAt {
		return nil, errors.New("the disk went away")
	}
	return f.bytesSource.ReadPiece(ctx, off, n)
}

// requests records what reaches the server, for the tests to read.
type requests struct {
	mu   sync.Mutex
	list []string
}

func (r *requests) hook(w http.ResponseWriter, req *http.Request) bool {
	r.mu.Lock()
	r.list = append(r.list, req.Method+" "+req.Header.Get("Content-Range")+req.Header.Get("Range"))
	r.mu.Unlock()
	return false
}

func (r *requests) take() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := strings.Join(r.list, "|")
	r.list = nil
	return out
}

func partsIn(t *testing.T, dir string) []string {
	t.Helper()
	m, _ := filepath.Glob(filepath.Join(dir, "*.part"))
	return m
}

// An upload that failed half way, by the same file again: it starts where
// the part ends, after reading back the part's tail, and the file arrives
// whole.
func TestUploadResumes(t *testing.T) {
	rec := &requests{}
	c, dir, _ := serveHook(t, rec.hook)
	c.Piece = 1 << 20
	ctx := context.Background()
	big := bytes.Repeat([]byte("0123456789abcdef"), 224<<10) // 3.5 MiB
	src := fpSource{bytesSource: big, fp: Fingerprint("big.bin", int64(len(big)), 42), failAt: 2 << 20}
	if err := c.Upload(ctx, "/files/big.bin", src, false, nil); err == nil {
		t.Fatal("the failing read did not fail the upload")
	}
	parts := partsIn(t, dir)
	if len(parts) != 1 || !strings.Contains(parts[0], src.fp) {
		t.Fatalf("the part was not kept: %v", parts)
	}
	if fi, _ := os.Stat(parts[0]); fi.Size() != 2<<20 {
		t.Fatalf("part of %d bytes", fi.Size())
	}
	rec.take()
	var first int64 = -1
	src.failAt = 0
	if err := c.Upload(ctx, "/files/big.bin", src, false, func(sent int64) {
		if first < 0 {
			first = sent
		}
	}); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "big.bin"))
	if !bytes.Equal(got, big) {
		t.Fatalf("%d bytes on disk", len(got))
	}
	// Stat, the tail read back, then only the last two pieces: no empty PUT.
	want := "PROPFIND |GET bytes=2031616-2097151|PUT bytes 2097152-3145727/3670016|PUT bytes 3145728-3670015/3670016|MOVE "
	if seen := rec.take(); seen != want {
		t.Fatalf("requests:\n %s\nwant\n %s", seen, want)
	}
	if first != 2<<20 || len(partsIn(t, dir)) != 0 {
		t.Fatalf("first progress %d, parts %v", first, partsIn(t, dir))
	}
}

// A part whose tail is not the file's -- another file with the same name,
// size and time, or a part somebody wrote to -- is started again, never
// continued: the result is the file, not a mixture.
func TestUploadDoesNotResumeAStrangersPart(t *testing.T) {
	c, dir, _ := serve(t)
	c.Piece = 1 << 20
	ctx := context.Background()
	big := bytes.Repeat([]byte("A"), 3<<20)
	src := fpSource{bytesSource: big, fp: "f00d", failAt: 2 << 20}
	_ = c.Upload(ctx, "/files/x.bin", src, false, nil)
	part := partsIn(t, dir)[0]
	// The part's last byte changes.
	b, _ := os.ReadFile(part)
	b[len(b)-1] = 'B'
	_ = os.WriteFile(part, b, 0o644)
	src.failAt = 0
	if err := c.Upload(ctx, "/files/x.bin", src, false, nil); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "x.bin")); !bytes.Equal(got, big) {
		t.Fatal("a stranger's part was continued")
	}
	// A part larger than the file is started again too.
	_ = os.WriteFile(filepath.Join(dir, "y.bin.f00d.part"), bytes.Repeat([]byte("A"), 4<<20), 0o644)
	if err := c.Upload(ctx, "/files/y.bin", src, false, nil); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "y.bin")); !bytes.Equal(got, big) {
		t.Fatalf("%d bytes", len(got))
	}
}

// What a failure leaves: a refusal removes the part (nothing to resume, and
// a full share wants its space back); a server failure keeps it; a Source
// that cannot say which file it is never leaves one.
func TestUploadFailureKeepsOrRemovesThePart(t *testing.T) {
	var answer int
	c, dir, _ := serveHook(t, func(w http.ResponseWriter, r *http.Request) bool {
		if answer != 0 && r.Method == http.MethodPut && strings.HasPrefix(r.Header.Get("Content-Range"), "bytes 1048576-") {
			w.WriteHeader(answer)
			return true
		}
		return false
	})
	c.Piece = 1 << 20
	ctx := context.Background()
	big := bytes.Repeat([]byte("z"), 3<<20)
	for _, tc := range []struct {
		status int
		src    Source
		kept   bool
	}{
		{http.StatusInsufficientStorage, fpSource{bytesSource: big, fp: "a"}, false},
		{http.StatusForbidden, fpSource{bytesSource: big, fp: "b"}, false},
		{http.StatusBadGateway, fpSource{bytesSource: big, fp: "c"}, true},
		{http.StatusBadGateway, bytesSource(big), false},
	} {
		answer = tc.status
		err := c.Upload(ctx, "/files/f.bin", tc.src, false, nil)
		var de *Error
		if !errors.As(err, &de) || de.Status != tc.status {
			t.Fatalf("%d: %v", tc.status, err)
		}
		if kept := len(partsIn(t, dir)) == 1; kept != tc.kept {
			t.Fatalf("%d, %T: part kept %v, want %v", tc.status, tc.src, kept, tc.kept)
		}
		for _, p := range partsIn(t, dir) {
			_ = os.Remove(p)
		}
	}
	// Cancelled by the person: nothing to come back to.
	answer = 0
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if err := c.Upload(cctx, "/files/g.bin", fpSource{bytesSource: big, fp: "d"}, false, nil); err == nil {
		t.Fatal("a cancelled upload went through")
	}
	if len(partsIn(t, dir)) != 0 {
		t.Fatal("a cancelled upload left its part")
	}
}

func TestStatAndFingerprint(t *testing.T) {
	c, dir, _ := serve(t)
	ctx := context.Background()
	_ = os.WriteFile(filepath.Join(dir, "a.txt"), []byte("12345"), 0o644)
	_ = os.Mkdir(filepath.Join(dir, "d"), 0o755)
	if e, err := c.Stat(ctx, "/files/a.txt"); err != nil || e.Size != 5 || e.Dir || e.Name != "a.txt" {
		t.Fatalf("%+v %v", e, err)
	}
	if e, err := c.Stat(ctx, "/files/d/"); err != nil || !e.Dir {
		t.Fatalf("%+v %v", e, err)
	}
	var de *Error
	if _, err := c.Stat(ctx, "/files/none"); !errors.As(err, &de) || de.Status != http.StatusNotFound {
		t.Fatalf("%v", err)
	}
	a, b := Fingerprint("x", 1, 2), Fingerprint("x", 1, 3)
	if len(a) != 16 || a == b || a != Fingerprint("x", 1, 2) || Fingerprint("x1", 1, 2) == Fingerprint("x", 11, 2) {
		t.Fatalf("%s %s", a, b)
	}
}
