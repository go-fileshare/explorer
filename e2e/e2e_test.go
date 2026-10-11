// SPDX-License-Identifier: BSD-3-Clause

// Package e2e runs the explorer as it is deployed: explorer.wasm in headless
// Chrome, served by the go-fileshare/portal binary over TLS, in front of a
// fileshare binary's WebDAV, with an OpenID provider written for the test. It is a
// module of its own so that the explorer does not depend on chromedp.
//
//	go install github.com/go-fileshare/portal@v0.2.0
//	go install github.com/go-fileshare/fileshare@v0.29.0
//	PORTAL=$(go env GOPATH)/bin/portal FILESHARE=$(go env GOPATH)/bin/fileshare go test -v ./...
//
// Skipped when PORTAL, FILESHARE or Chrome is missing.
package e2e

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chromedp/cdproto/browser"
	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/chromedp"
)

type lockedBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// start runs a binary and returns its output, and the first submatch of
// ready once a line matches it.
func start(t *testing.T, ready *regexp.Regexp, name string, args ...string) (*lockedBuffer, string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, name, args...)
	out := &lockedBuffer{}
	pr, pw := io.Pipe()
	cmd.Stdout, cmd.Stderr = io.MultiWriter(out, pw), io.MultiWriter(out, pw)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); _ = cmd.Wait(); pw.Close() })
	found := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(pr)
		for sc.Scan() {
			if m := ready.FindStringSubmatch(sc.Text()); m != nil {
				found <- m[1]
			}
		}
	}()
	select {
	case s := <-found:
		return out, s
	case <-time.After(30 * time.Second):
		t.Fatalf("%s never became ready:\n%s", filepath.Base(name), out)
	}
	return nil, ""
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func selfSigned(t *testing.T, dir string) (string, string) {
	t.Helper()
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "localhost"},
		DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true, IsCA: true}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	kd, _ := x509.MarshalPKCS8PrivateKey(k)
	cert, key := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	_ = os.WriteFile(cert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644)
	_ = os.WriteFile(key, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kd}), 0o600)
	return cert, key
}

// buildExplorer builds the explorer for the browser, with the test's hook
// (-tags e2e), into a directory the portal serves as its "explorer" UI.
func buildExplorer(t *testing.T) string {
	t.Helper()
	dist := t.TempDir()
	b := exec.Command("go", "build", "-tags", "e2e", "-o", filepath.Join(dist, "explorer.wasm"), ".")
	b.Dir = ".." // the explorer's own module
	b.Env = append(os.Environ(), "GOOS=js", "GOARCH=wasm", "CGO_ENABLED=0")
	if out, err := b.CombinedOutput(); err != nil {
		t.Fatalf("building the explorer: %v\n%s", err, out)
	}
	execJS, err := os.ReadFile(filepath.Join(runtime.GOROOT(), "lib", "wasm", "wasm_exec.js"))
	if err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(dist, "wasm_exec.js"), execJS, 0o644)
	web, _ := filepath.Glob("../web/*")
	for _, f := range web {
		b, _ := os.ReadFile(f)
		_ = os.WriteFile(filepath.Join(dist, filepath.Base(f)), b, 0o644)
	}
	return dist
}

var fileshareReady = regexp.MustCompile(`https://(127\.0\.0\.1:\d+) — the admin API, for OIDC tokens`)

type fileshare struct {
	addr, audience, data string
	dav, files           string
	out                  *lockedBuffer
}

var davLine = regexp.MustCompile(`webdav\s+on (?:https://)?(127\.0\.0\.1:\d+)`)

func startFileshare(t *testing.T, bin string, idp *testIdP, name, audience, cert, key string) *fileshare {
	t.Helper()
	dir := t.TempDir()
	sock, err := os.MkdirTemp("", "fs")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(sock) })
	roots := filepath.Join(dir, "roots")
	_ = os.MkdirAll(filepath.Join(roots, "data"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, "alice.pw"), []byte("correct horse battery staple\n"), 0o600)
	files := filepath.Join(dir, "files")
	_ = os.MkdirAll(filepath.Join(files, "reports"), 0o755)
	_ = os.WriteFile(filepath.Join(files, "readme.txt"), []byte("hello"), 0o644)
	cfg := fmt.Sprintf(`name = %q
user "alice" { password_file = %q }
tls {
  cert_file = %q
  key_file  = %q
}
oidc {
  issuer      = %q
  audience    = %q
  local_names = true
}
share "files" {
  directory = %q
  allow     = ["alice"]
  writers   = ["alice"]
}
serve "webdav" {
  addr = "127.0.0.1:0"
  tls  = true
}
admin {
  listen       = "unix://%s/admin.sock"
  state_file   = %q
  source_roots = [%q]
  web {
    listen = "127.0.0.1:0"
    issuer %q {
      audience = %q
      groups   = ["fileshare-admins"]
    }
  }
}
`, name, filepath.Join(dir, "alice.pw"), cert, key, idp.issuer, audience+"dav", files, sock, filepath.Join(dir, "shares.json"), roots, idp.issuer, audience)
	path := filepath.Join(dir, "fileshare.hcl")
	_ = os.WriteFile(path, []byte(cfg), 0o644)
	out, addr := start(t, fileshareReady, bin, "serve", "--config", path)
	dav := davLine.FindStringSubmatch(out.String())
	if dav == nil {
		t.Fatalf("%s serves no WebDAV:\n%s", name, out)
	}
	return &fileshare{addr: addr, audience: audience, data: filepath.Join(roots, "data"), out: out, dav: dav[1], files: files}
}

type state struct {
	User    string   `json:"user"`
	Status  string   `json:"status"`
	Dir     string   `json:"dir"`
	Entries []string `json:"entries"`
	Busy    bool     `json:"busy"`
}

func TestE2EExplorerInABrowserBehindThePortal(t *testing.T) {
	portalBin, fsBin := os.Getenv("PORTAL"), os.Getenv("FILESHARE")
	if portalBin == "" || fsBin == "" {
		t.Skip("PORTAL and FILESHARE name the binaries to test against")
	}
	dir := t.TempDir()
	cert, key := selfSigned(t, dir)
	idp := newTestIdP(t)
	port := freePort(t)
	public := fmt.Sprintf("https://127.0.0.1:%d", port)
	idp.redirect = public + "/callback"
	a := startFileshare(t, fsBin, idp, "A", "https://fs-a.test/", cert, key)
	idp.resources = []string{a.audience, a.audience + "dav"}

	dist := buildExplorer(t)
	_ = os.WriteFile(filepath.Join(dir, "session.key"), []byte(strings.Repeat("k", 32)), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "client.secret"), []byte(idp.secret), 0o600)
	pcfg := fmt.Sprintf(`
listen     = "127.0.0.1:%d"
public_url = %q
tls {
  cert_file = %q
  key_file  = %q
}
session { key_files = [%q] }
issuer %q {
  client_id          = "portal"
  client_secret_file = %q
}
server "a" {
  label        = "Paris"
  url          = "https://%s"
  issuer       = %q
  resource     = %q
  ca_file      = %q
  dav_url      = "https://%s"
  dav_resource = %q
}
ui "explorer" { dir = %q }
`, port, public, cert, key, filepath.Join(dir, "session.key"), idp.issuer, filepath.Join(dir, "client.secret"),
		a.addr, idp.issuer, a.audience, cert, a.dav, a.audience+"dav", dist)
	ppath := filepath.Join(dir, "portal.hcl")
	_ = os.WriteFile(ppath, []byte(pcfg), 0o644)
	portalOut, _ := start(t, regexp.MustCompile(`portal: serving (\S+)`), portalBin, "-config", ppath)

	opts := append(chromedp.DefaultExecAllocatorOptions[:], chromedp.Flag("headless", true),
		chromedp.Flag("ignore-certificate-errors", true))
	if p := os.Getenv("CHROME"); p != "" {
		opts = append(opts, chromedp.ExecPath(p))
	}
	actx, cancel := chromedp.NewExecAllocator(context.Background(), opts...)
	defer cancel()
	ctx, cancel2 := chromedp.NewContext(actx)
	defer cancel2()
	ctx, cancel3 := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel3()
	if _, err := chromedp.Run(ctx, chromedp.Navigate("about:blank")); err != nil {
		t.Skipf("no Chrome: %v", err)
	}
	downloads := t.TempDir()
	if err := chromedp.Do(ctx, chromedp.EmulateViewport(1280, 800),
		chromedp.Func(func(ctx context.Context, tg *chromedp.Target) error {
			_, err := cdp.Call(ctx, tg, browser.SetDownloadBehavior, browser.SetDownloadBehaviorParams{
				Behavior: browser.SetDownloadBehaviorBehaviorAllow, DownloadPath: downloads})
			return err
		})); err != nil {
		t.Fatal(err)
	}
	do := func(js string) {
		t.Helper()
		if _, err := chromedp.Run(ctx, chromedp.Evaluate[chromedp.Void](js)); err != nil {
			t.Fatalf("%s: %v", js, err)
		}
	}
	idle := func(s state) bool { return !s.Busy }

	// Open the explorer with no session: it sends the browser to log in, and
	// comes back logged in, at the root -- the shares alice may reach.
	if _, err := chromedp.Run(ctx, chromedp.Navigate(public+"/explorer/")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, ctx, "the explorer logged in, listing the shares", func(s state) bool {
		return s.User == "alice" && s.Dir == "/" && slices.Equal(s.Entries, []string{"files"})
	}, portalOut)

	// Into the share: what is on fileshare's disk is what the page lists.
	do(`explorerDo("open", "", 0)`)
	waitFor(t, ctx, "the share listed", func(s state) bool {
		return s.Dir == "/files/" && slices.Equal(s.Entries, []string{"reports", "readme.txt"})
	}, portalOut)

	// Upload: the explorer makes a file input; the test hands it a file, as
	// a person would through the picker. 20 MiB: three pieces, more than one
	// request the portal takes.
	src := filepath.Join(t.TempDir(), "big.bin")
	big := []byte(strings.Repeat("explorer", 20<<17))
	_ = os.WriteFile(src, big, 0o644)
	do(`explorerDo("upload")`)
	if _, err := chromedp.Run(ctx, chromedp.SetUploadFiles(`input[type=file]`, []string{src}, chromedp.NodeReady)); err != nil {
		t.Fatal(err)
	}
	st := waitFor(t, ctx, "the upload done", func(s state) bool {
		return idle(s) && slices.Contains(s.Entries, "big.bin")
	}, portalOut)
	if !strings.HasPrefix(st.Status, "1 uploaded") {
		t.Fatalf("status %q", st.Status)
	}
	if got, err := os.ReadFile(filepath.Join(a.files, "big.bin")); err != nil || string(got) != string(big) {
		t.Fatalf("on disk: %d bytes, %v", len(got), err)
	}
	if left, _ := filepath.Glob(filepath.Join(a.files, "*.part")); len(left) != 0 {
		t.Fatalf("part files left: %v", left)
	}

	// A folder, then a rename onto an existing name, refused.
	do(`explorerDo("newFolder", "drafts")`)
	waitFor(t, ctx, "the folder made", func(s state) bool { return idle(s) && slices.Contains(s.Entries, "drafts") }, portalOut)
	if fi, err := os.Stat(filepath.Join(a.files, "drafts")); err != nil || !fi.IsDir() {
		t.Fatalf("drafts: %v", err)
	}
	i := slices.Index(waitFor(t, ctx, "idle", idle, portalOut).Entries, "big.bin")
	do(fmt.Sprintf(`explorerDo("rename", "readme.txt", %d)`, i))
	waitFor(t, ctx, "the rename refused", func(s state) bool { return idle(s) && strings.Contains(s.Status, "there already") }, portalOut)
	if b, _ := os.ReadFile(filepath.Join(a.files, "readme.txt")); string(b) != "hello" {
		t.Fatal("a refused rename replaced the file")
	}

	// Download: the browser follows a link to the portal, which serves the
	// file as an attachment; it lands in the download directory whole.
	i = slices.Index(waitFor(t, ctx, "idle", idle, portalOut).Entries, "readme.txt")
	do(fmt.Sprintf(`explorerDo("download", "", %d)`, i))
	var got []byte
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		if got, _ = os.ReadFile(filepath.Join(downloads, "readme.txt")); got != nil {
			break
		}
	}
	if string(got) != "hello" {
		names, _ := os.ReadDir(downloads)
		t.Fatalf("downloaded %q; the directory holds %v", got, names)
	}
	capture(t, ctx)

	// Delete takes two presses; then it is gone from the disk.
	i = slices.Index(waitFor(t, ctx, "idle", idle, portalOut).Entries, "drafts")
	do(fmt.Sprintf(`explorerDo("delete", "", %d)`, i))
	do(`explorerDo("delete")`)
	waitFor(t, ctx, "the folder deleted", func(s state) bool { return idle(s) && !slices.Contains(s.Entries, "drafts") }, portalOut)
	if _, err := os.Stat(filepath.Join(a.files, "drafts")); !os.IsNotExist(err) {
		t.Fatalf("drafts still on disk: %v", err)
	}
}

// waitFor polls the explorer's state until ok, or fails with what it last saw.
func waitFor(t *testing.T, ctx context.Context, what string, ok func(state) bool, logs *lockedBuffer) state {
	t.Helper()
	var last state
	var lastErr error
	for deadline := time.Now().Add(60 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		raw, err := chromedp.Run(ctx, chromedp.Evaluate[string](`typeof explorerState === "function" ? explorerState() : "{}"`))
		if err != nil {
			lastErr = err // a navigation in progress
			continue
		}
		var s state
		if json.Unmarshal([]byte(raw), &s) == nil {
			last = s
			if ok(s) {
				return s
			}
		}
	}
	url, _ := chromedp.Run(ctx, chromedp.Evaluate[string](`location.href`))
	t.Fatalf("%s: never. Last state %+v, last error %v, page %s\nportal:\n%s", what, last, lastErr, url, logs)
	return last
}

// capture writes a picture of the page to $EXPLORER_SCREENSHOT, when set, so
// that a person can look at what the test saw. Never inside a git work tree:
// a capture one "git add -A" away from a public repository is how pictures
// get published by accident, and an ignore rule is no barrier.
func capture(t *testing.T, ctx context.Context) {
	t.Helper()
	path := os.Getenv("EXPLORER_SCREENSHOT")
	if path == "" {
		return
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		t.Fatal(err)
	}
	for d := filepath.Dir(abs); ; d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(d, ".git")); err == nil {
			t.Fatalf("EXPLORER_SCREENSHOT %s is inside the git work tree %s: write it elsewhere", abs, d)
		}
		if filepath.Dir(d) == d {
			break
		}
	}
	png, err := chromedp.Run(ctx, chromedp.FullScreenshot(100))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(abs, png, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("screenshot: %s", abs)
}
