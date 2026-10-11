// SPDX-License-Identifier: BSD-3-Clause

//go:build !(js && wasm)

package direct

import (
	"bytes"
	"context"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/go-authn/oidc"
	"github.com/go-fileshare/portal/client"
	"github.com/go-filesystems/osfs"
	"github.com/go-filesystems/webdav"
)

// world is a native explorer's: a provider, two servers whose WebDAV verifies
// its token as fileshare does (go-authn/oidc, each its own audience) over
// TLS, and a browser that logs in when asked.
type world struct {
	idp     *testIdP
	b       *Backend
	dirs    map[string]string
	opened  atomic.Int64
	refused atomic.Int64
	// refuseNext makes the next request to a server fail as an expired
	// token would.
	refuseNext atomic.Bool
}

func newWorld(t *testing.T) *world {
	t.Helper()
	w := &world{idp: newTestIdP(t), dirs: map[string]string{}}
	w.idp.public = true
	w.idp.resources = []string{"https://a.example/", "https://b.example/"}
	ca := filepath.Join(t.TempDir(), "ca.pem")
	serve := func(id, aud string) string {
		dir := t.TempDir()
		_ = os.MkdirAll(filepath.Join(dir, "docs"), 0o755)
		_ = os.WriteFile(filepath.Join(dir, "readme.txt"), []byte("hello "+id), 0o644)
		w.dirs[id] = dir
		fsys, err := osfs.Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = fsys.Close() })
		h, err := webdav.New(fsys, webdav.WithPrefix("/files"), webdav.ReadWrite())
		if err != nil {
			t.Fatal(err)
		}
		v, err := oidc.New(context.Background(), oidc.Config{Issuer: w.idp.issuer, Audience: aud})
		if err != nil {
			t.Fatal(err)
		}
		srv := httptest.NewTLSServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
			raw, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
			if _, err := v.Verify(r.Context(), raw); err != nil || w.refuseNext.CompareAndSwap(true, false) {
				w.refused.Add(1)
				rw.WriteHeader(http.StatusUnauthorized)
				return
			}
			h.ServeHTTP(rw, r)
		}))
		t.Cleanup(srv.Close)
		f, _ := os.OpenFile(ca, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		_ = pem.Encode(f, &pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
		f.Close()
		return srv.URL
	}
	aURL, bURL := serve("a", "https://a.example/"), serve("b", "https://b.example/")
	cfg := Config{Issuer: w.idp.issuer, ClientID: w.idp.clientID, Servers: []Server{
		{ID: "a", Label: "Paris", DAVURL: aURL, Resource: "https://a.example/", CAFile: ca},
		{ID: "b", DAVURL: bURL, Resource: "https://b.example/", CAFile: ca},
	}}
	b, err := New(cfg, w.browser(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	w.b = b
	return w
}

// browser opens the URL as the system browser would: the provider's login,
// then its redirect to the explorer's loopback listener.
func (w *world) browser(t *testing.T) func(string) error {
	return func(auth string) error {
		w.opened.Add(1)
		go func() {
			nc := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
			res, err := nc.Get(auth)
			if err != nil {
				t.Error(err)
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
}

type bytesSource []byte

func (s bytesSource) Size() int64 { return int64(len(s)) }
func (s bytesSource) ReadPiece(_ context.Context, off, n int64) ([]byte, error) {
	return s[off : off+n], nil
}

func TestLoginBrowseUploadDownload(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	s, err := w.b.Session(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if s.Who() != "alice" || len(s.Servers) != 2 || !s.Servers[0].DAV || !s.Servers[0].Reachable || s.Servers[0].Name() != "Paris" {
		t.Fatalf("%+v", s)
	}
	// Logged in once: a second Session does not open the browser again.
	if _, err := w.b.Session(ctx); err != nil || w.opened.Load() != 1 {
		t.Fatalf("%v, browser opened %d times", err, w.opened.Load())
	}
	for _, id := range []string{"a", "b"} {
		es, err := w.b.DAV(id).List(ctx, "/files/")
		if err != nil || len(es) != 2 || es[0].Name != "docs" {
			t.Fatalf("%s: %v %+v", id, err, es)
		}
	}
	// 20 MiB in pieces of 8, then back.
	big := bytes.Repeat([]byte("explorer"), 20<<17)
	if err := w.b.DAV("a").Upload(ctx, "/files/big.bin", bytesSource(big), false, nil); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(w.dirs["a"], "big.bin")); !bytes.Equal(got, big) {
		t.Fatalf("on disk: %d bytes", len(got))
	}
	rc, err := w.b.DAV("a").Get(ctx, "/files/big.bin")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(got, big) {
		t.Fatalf("downloaded %d bytes", len(got))
	}
	// Each server got a token for itself alone: none was refused, and one
	// refresh bought each.
	if w.refused.Load() != 0 || w.idp.refreshN != 2 {
		t.Fatalf("%d refused, %d refreshes", w.refused.Load(), w.idp.refreshN)
	}
	if err := w.b.EnsureToken(ctx, "a"); err != nil || w.idp.refreshN != 2 {
		t.Fatalf("a current token was bought again: %v, %d", err, w.idp.refreshN)
	}
}

// A server that refuses the token (expired early, revoked) gets a new one,
// once, and the request goes through.
func TestARefusedTokenIsRefreshedOnce(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	if _, err := w.b.Session(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := w.b.DAV("a").List(ctx, "/files/"); err != nil {
		t.Fatal(err)
	}
	before := w.idp.refreshN
	w.refuseNext.Store(true)
	if _, err := w.b.DAV("a").List(ctx, "/files/"); err != nil {
		t.Fatal(err)
	}
	if w.refused.Load() != 1 || w.idp.refreshN != before+1 {
		t.Fatalf("%d refused, %d refreshes", w.refused.Load(), w.idp.refreshN-before)
	}
}

// Many requests at once, to two servers, with no token yet: the refresh
// token is spent one refresh at a time. Spent twice, the provider would end
// the login (its rotation's reuse detection).
func TestConcurrentRequestsSpendTheRefreshTokenOnce(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	if _, err := w.b.Session(ctx); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 40)
	for i := range 40 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := w.b.DAV([]string{"a", "b"}[i%2]).List(ctx, "/files/")
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if w.idp.refreshN != 2 {
		t.Fatalf("%d refreshes for two servers", w.idp.refreshN)
	}
}

// A login the provider ended: ErrNotLoggedIn, which the explorer reads as
// "log in again"; and a logout forgets everything.
func TestEndedLoginAndLogout(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	if _, err := w.b.Session(ctx); err != nil {
		t.Fatal(err)
	}
	w.idp.mu.Lock()
	for _, fam := range w.idp.live {
		w.idp.revoked[fam] = true
	}
	w.idp.mu.Unlock()
	if _, err := w.b.DAV("a").List(ctx, "/files/"); !errors.Is(err, client.ErrNotLoggedIn) {
		t.Fatalf("an ended login: %v", err)
	}
	// Logging in again opens the browser again.
	if _, err := w.b.Session(ctx); err != nil || w.opened.Load() != 2 {
		t.Fatalf("%v, %d", err, w.opened.Load())
	}
	if _, err := w.b.DAV("a").List(ctx, "/files/"); err != nil {
		t.Fatal(err)
	}
	if _, err := w.b.Logout(ctx); err != nil {
		t.Fatal(err)
	}
	if err := w.b.EnsureToken(ctx, "a"); !errors.Is(err, client.ErrNotLoggedIn) {
		t.Fatalf("after logout: %v", err)
	}
	if w.b.LoginURL("/x") != LoginAgain {
		t.Fatal(w.b.LoginURL("/x"))
	}
}

func TestRefusals(t *testing.T) {
	ctx := context.Background()
	for name, cfg := range map[string]Config{
		"no issuer":  {ClientID: "c", Servers: []Server{{ID: "a", Resource: "r"}}},
		"no client":  {Issuer: "https://i", Servers: []Server{{ID: "a", Resource: "r"}}},
		"no server":  {Issuer: "https://i", ClientID: "c"},
		"no address": {Issuer: "https://i", ClientID: "c", Servers: []Server{{ID: "a", DAVURL: "https://a"}}},
	} {
		if _, err := New(cfg, nil, nil); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	w := newWorld(t)
	if _, err := w.b.DAV("nope").List(ctx, "/"); err == nil || !strings.Contains(err.Error(), `no server "nope"`) {
		t.Errorf("an unknown server: %v", err)
	}
	if err := w.b.EnsureToken(ctx, "nope"); err == nil {
		t.Error("EnsureToken of an unknown server")
	}
	// Not logged in yet: nothing is sent.
	if _, err := w.b.DAV("a").List(ctx, "/files/"); !errors.Is(err, client.ErrNotLoggedIn) {
		t.Errorf("before login: %v", err)
	}
	// A CA file that is not there, or holds no certificate.
	empty := filepath.Join(t.TempDir(), "empty.pem")
	_ = os.WriteFile(empty, []byte("nothing"), 0o644)
	for _, ca := range []string{filepath.Join(t.TempDir(), "none.pem"), empty} {
		b, _ := New(Config{Issuer: "https://i", ClientID: "c", Servers: []Server{{ID: "a", DAVURL: "https://a", Resource: "r", CAFile: ca}}}, nil, nil)
		if _, err := b.DAV("a").List(ctx, "/"); err == nil {
			t.Errorf("CA %s: accepted", ca)
		}
	}
	// A body larger than a request may be is refused before anything is sent.
	if _, err := w.b.Session(ctx); err != nil {
		t.Fatal(err)
	}
	big := bytesSource(make([]byte, MaxBody+1))
	c := w.b.DAV("a")
	c.Piece = MaxBody + 1
	if err := c.Upload(ctx, "/files/huge", big, false, nil); !errors.Is(err, client.ErrTooLarge) {
		t.Errorf("an oversized piece: %v", err)
	}
	// A provider that is not there.
	b, _ := New(Config{Issuer: "http://127.0.0.1:1", ClientID: "c", Servers: []Server{{ID: "a", Resource: "r"}}}, nil, nil)
	if _, err := b.Session(ctx); err == nil {
		t.Error("a provider that is not there")
	}
}

// A provider that issues no refresh token cannot serve several servers:
// said at login, rather than at the first call.
func TestNoRefreshToken(t *testing.T) {
	w := newWorld(t)
	w.idp.noRefresh = true
	if _, err := w.b.Session(context.Background()); err == nil || !strings.Contains(err.Error(), "offline_access") {
		t.Fatalf("%v", err)
	}
}

// A server that redirects is not followed. Go's client keeps Authorization
// on a redirect to the same host -- or a subdomain -- whatever the scheme, so
// following one to http:// would send the token in clear. The listener on the
// other end records what reaches it.
func TestARedirectIsNotFollowed(t *testing.T) {
	var leaked atomic.Value
	clear := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		leaked.Store(r.Header.Get("Authorization"))
	}))
	defer clear.Close()
	w := newWorld(t)
	ctx := context.Background()
	if _, err := w.b.Session(ctx); err != nil {
		t.Fatal(err)
	}
	// The TLS server of "a" answers this one request with a redirect to the
	// cleartext listener, on the same host (127.0.0.1).
	hop := httptest.NewTLSServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		http.Redirect(rw, r, clear.URL+r.URL.Path, http.StatusFound)
	}))
	defer hop.Close()
	sv := w.b.cfg.Servers[0]
	sv.ID, sv.DAVURL, sv.CAFile = "hop", hop.URL, ""
	hc, _ := serverClient(sv)
	hc.Transport.(*http.Transport).TLSClientConfig.InsecureSkipVerify = true //nolint:gosec // the test's own server
	c := &doer{b: w.b, sv: w.b.cfg.Servers[0], hc: hc}
	req, _ := http.NewRequestWithContext(ctx, "PROPFIND", hop.URL+"/files/", nil)
	res, err := c.Do(req)
	if err == nil {
		res.Body.Close()
	}
	if v, _ := leaked.Load().(string); v != "" {
		t.Fatalf("the token was sent in clear after a redirect: %.20s...", v)
	}
	if err != nil || res.StatusCode != http.StatusFound {
		t.Fatalf("want the redirect answered as it is: %v %v", err, res)
	}
}
