// SPDX-License-Identifier: BSD-3-Clause

package e2e

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-fileshare/explorer/internal/dav"
	"github.com/go-fileshare/explorer/internal/direct"
)

type bytesSource []byte

func (s bytesSource) Size() int64 { return int64(len(s)) }
func (s bytesSource) ReadPiece(_ context.Context, off, n int64) ([]byte, error) {
	return s[off : off+n], nil
}

// The native explorer's backend -- a public client logging in through a
// loopback listener (go-authn/rp, RFC 8252) and calling the server's WebDAV
// with a token for its oidc audience -- against a REAL fileshare. No window:
// what is tested is that real fileshare takes what the native explorer
// sends, and refuses a token meant for another audience.
func TestE2ENativeAgainstARealServer(t *testing.T) {
	fsBin := os.Getenv("FILESHARE")
	if fsBin == "" {
		t.Skip("FILESHARE names the fileshare binary to test against")
	}
	dir := t.TempDir()
	cert, key := selfSigned(t, dir)
	idp := newTestIdP(t)
	idp.public = true
	a := startFileshare(t, fsBin, idp, "A", "https://fs-a.test/", cert, key)
	idp.resources = []string{a.audience, a.audience + "dav"}

	browser := func(auth string) error {
		go func() {
			nc := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
			res, err := nc.Get(auth)
			if err != nil {
				return
			}
			res.Body.Close()
			back, _ := url.Parse(res.Header.Get("Location"))
			if res, err := http.Get(back.String()); err == nil {
				res.Body.Close()
			}
		}()
		return nil
	}
	b, err := direct.New(direct.Config{Issuer: idp.issuer, ClientID: idp.clientID, Servers: []direct.Server{
		{ID: "a", DAVURL: "https://" + a.dav, Resource: a.audience + "dav", CAFile: cert},
		// CONTROL: the same WebDAV, with a token for the admin API's audience.
		{ID: "wrong", DAVURL: "https://" + a.dav, Resource: a.audience, CAFile: cert},
	}}, browser, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	s, err := b.Session(ctx)
	if err != nil || s.Who() != "alice" {
		t.Fatalf("%v %+v", err, s)
	}
	c := b.DAV("a")
	shares, err := c.List(ctx, "/")
	if err != nil || len(shares) != 1 || shares[0].Name != "files" || !shares[0].Dir {
		t.Fatalf("the shares: %v %+v", err, shares)
	}
	big := bytes.Repeat([]byte("explorer"), 20<<17) // 20 MiB: three pieces
	if err := c.Upload(ctx, "/files/big.bin", bytesSource(big), false, nil); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(a.files, "big.bin")); !bytes.Equal(got, big) {
		t.Fatalf("on disk: %d bytes", len(got))
	}
	rc, err := c.Get(ctx, "/files/big.bin")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(got, big) {
		t.Fatalf("downloaded %d bytes", len(got))
	}
	if err := c.Move(ctx, "/files/big.bin", "/files/readme.txt"); !dav.Exists(err) {
		t.Fatalf("a move onto a file: %v", err)
	}
	var de *dav.Error
	if _, err := b.DAV("wrong").List(ctx, "/files/"); !errors.As(err, &de) || de.Status != http.StatusUnauthorized {
		t.Fatalf("CONTROL: a token for another audience was not refused: %v", err)
	}
}
