// SPDX-License-Identifier: BSD-3-Clause

//go:build !(js && wasm)

// Package direct is the native explorer's way to the servers: no portal. It
// logs in itself -- the system browser and a loopback listener, as a public
// client (go-authn/rp, RFC 8252) -- and calls each server's WebDAV with a
// token addressed to that server alone (RFC 8707). A browser page goes
// through the portal instead, and never links any of this.
package direct

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/go-authn/rp"
	"github.com/go-fileshare/portal/client"

	"github.com/go-fileshare/explorer/internal/dav"
)

// Config is what the native explorer needs to reach servers on its own:
// where to log in, and the servers -- what a portal would know.
type Config struct {
	Issuer   string
	ClientID string // a PUBLIC client: no secret (RFC 8252 §8.5)
	Scopes   []string
	Servers  []Server
}

// Server is one fileshare server's WebDAV.
type Server struct {
	ID, Label string
	DAVURL    string // its WebDAV, over https
	Resource  string // RFC 8707: the audience of its oidc block
	Scope     string // for a provider that sets the audience from a scope
	CAFile    string // when its certificate is not publicly trusted
}

// LoginAgain is what LoginURL returns: a native window has no page to go to;
// its host logs in again when told to navigate there.
const LoginAgain = "native:login"

// MaxBody bounds one request body, kept to be sent again after a refresh:
// the explorer's upload pieces are 8 MiB.
const MaxBody = 16 << 20

// Backend logs in once, keeps the refresh token in this process's memory
// only, and spends it one refresh at a time: a provider rotates a public
// client's refresh tokens and ends the login when one is spent twice.
type Backend struct {
	cfg  Config
	open func(string) error
	hc   *http.Client // for the provider
	now  func() time.Time

	mu      sync.Mutex // the login, the refresh token and the tokens
	rpc     *rp.Client
	refresh string
	who     *client.Session
	tokens  map[string]rp.Token

	dmu  sync.Mutex
	davs map[string]*dav.Client
}

// New is a Backend over cfg. open shows a URL to the person: the system
// browser (rp.OpenBrowser).
func New(cfg Config, open func(string) error, hc *http.Client) (*Backend, error) {
	if cfg.Issuer == "" || cfg.ClientID == "" {
		return nil, errors.New("explorer: an issuer and its client_id are needed to log in")
	}
	if len(cfg.Servers) == 0 {
		return nil, errors.New("explorer: no server to browse")
	}
	for _, s := range cfg.Servers {
		if s.Resource == "" && s.Scope == "" {
			return nil, fmt.Errorf("explorer: server %q: neither resource nor scope: its token would not be addressed to it alone", s.ID)
		}
	}
	if hc == nil {
		hc = http.DefaultClient
	}
	if len(cfg.Scopes) == 0 {
		cfg.Scopes = []string{"openid", "profile", "offline_access"}
	}
	return &Backend{cfg: cfg, open: open, hc: hc, now: time.Now, tokens: map[string]rp.Token{},
		davs: map[string]*dav.Client{}}, nil
}

// Session logs in, the first time, and says who and which servers.
func (b *Backend) Session(ctx context.Context) (*client.Session, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.who != nil {
		return b.who, nil
	}
	p, err := rp.Discover(ctx, b.hc, b.cfg.Issuer)
	if err != nil {
		return nil, err
	}
	c, tok, id, err := rp.Loopback(ctx, p, rp.Config{ClientID: b.cfg.ClientID, Scopes: b.cfg.Scopes}, b.open, nil)
	if err != nil {
		return nil, err
	}
	if tok.RefreshToken == "" {
		return nil, errors.New("explorer: the provider issued no refresh token; allow refresh tokens (offline_access) for this client")
	}
	s := &client.Session{Issuer: b.cfg.Issuer, Subject: id.Subject(), Name: id.Username()}
	for _, sv := range b.cfg.Servers {
		s.Servers = append(s.Servers, client.Server{ID: sv.ID, Label: sv.Label, Reachable: true, DAV: true})
	}
	b.rpc, b.refresh, b.who = c, tok.RefreshToken, s
	return s, nil
}

// LoginURL is LoginAgain: there is no page to send a native window to.
func (b *Backend) LoginURL(string) string { return LoginAgain }

// Logout forgets the login, and returns where to end the session at the
// provider, if it says.
func (b *Backend) Logout(context.Context) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	end := ""
	if b.rpc != nil {
		end = b.rpc.EndSessionURL("")
	}
	b.forget()
	return end, nil
}

func (b *Backend) forget() {
	b.rpc, b.refresh, b.who = nil, "", nil
	b.tokens = map[string]rp.Token{}
}

// EnsureToken makes sure a current token for server is held.
func (b *Backend) EnsureToken(ctx context.Context, server string) error {
	sv, ok := b.server(server)
	if !ok {
		return fmt.Errorf("explorer: no server %q", server)
	}
	_, err := b.token(ctx, sv, false)
	return err
}

func (b *Backend) server(id string) (Server, bool) {
	i := slices.IndexFunc(b.cfg.Servers, func(s Server) bool { return s.ID == id })
	if i < 0 {
		return Server{}, false
	}
	return b.cfg.Servers[i], true
}

// token is a current access token for sv, refreshed when it is missing,
// expiring, or drop is set (the server refused the last one).
func (b *Backend) token(ctx context.Context, sv Server, drop bool) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.rpc == nil {
		return "", client.ErrNotLoggedIn
	}
	if t, ok := b.tokens[sv.ID]; ok && !drop && (t.Expiry.IsZero() || b.now().Add(30*time.Second).Before(t.Expiry)) {
		return t.AccessToken, nil
	}
	t, err := b.rpc.Refresh(ctx, b.refresh, rp.For{Resource: sv.Resource, Scope: sv.Scope})
	if err != nil {
		if rp.IsInvalidGrant(err) {
			b.forget()
			return "", client.ErrNotLoggedIn
		}
		return "", err
	}
	if t.RefreshToken != "" {
		b.refresh = t.RefreshToken
	}
	b.tokens[sv.ID] = *t
	return t.AccessToken, nil
}

// DAV is server's WebDAV, called directly with its own token.
func (b *Backend) DAV(server string) *dav.Client {
	b.dmu.Lock()
	defer b.dmu.Unlock()
	if c, ok := b.davs[server]; ok {
		return c
	}
	var c *dav.Client
	sv, ok := b.server(server)
	if !ok {
		c = dav.New("https://invalid", failing{fmt.Errorf("explorer: no server %q", server)})
	} else if hc, err := serverClient(sv); err != nil {
		c = dav.New(sv.DAVURL, failing{err})
	} else {
		c = dav.New(sv.DAVURL, &doer{b: b, sv: sv, hc: hc})
	}
	b.davs[server] = c
	return c
}

// serverClient is the HTTP client of one server: its CA when it has one.
// No overall timeout: a download streams for as long as it takes.
//
// ⛔ It follows no redirect. Go's client keeps Authorization on a redirect
// to the same host or a subdomain whatever the scheme, so a 302 to http://
// -- a misconfigured proxy in front of the server -- would send the token in
// clear. WebDAV has no use for one; it is answered as it is, an error.
func serverClient(sv Server) (*http.Client, error) {
	tc := &tls.Config{MinVersion: tls.VersionTLS12}
	if sv.CAFile != "" {
		pem, err := os.ReadFile(sv.CAFile)
		if err != nil {
			return nil, fmt.Errorf("server %q: %w", sv.ID, err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("server %q: %s holds no certificate", sv.ID, sv.CAFile)
		}
		tc.RootCAs = pool
	}
	return &http.Client{CheckRedirect: noRedirect, Transport: &http.Transport{TLSClientConfig: tc, ForceAttemptHTTP2: true,
		Proxy: http.ProxyFromEnvironment, TLSHandshakeTimeout: 10 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second}}, nil
}

func noRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// doer adds the server's token and, when the server refuses it (401),
// refreshes once and sends the request again.
type doer struct {
	b  *Backend
	sv Server
	hc *http.Client
}

func (d *doer) Do(req *http.Request) (*http.Response, error) {
	var body []byte
	if req.Body != nil {
		bb, err := io.ReadAll(io.LimitReader(req.Body, MaxBody+1))
		req.Body.Close()
		if err != nil {
			return nil, err
		}
		if len(bb) > MaxBody {
			return nil, client.ErrTooLarge
		}
		body = bb
	}
	send := func(drop bool) (*http.Response, error) {
		tok, err := d.b.token(req.Context(), d.sv, drop)
		if err != nil {
			return nil, err
		}
		r := req.Clone(req.Context())
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))
		r.Header.Set("Authorization", "Bearer "+tok)
		return d.hc.Do(r)
	}
	res, err := send(false)
	if err != nil || res.StatusCode != http.StatusUnauthorized {
		return res, err
	}
	res.Body.Close()
	return send(true)
}

// failing answers every request with err: a server that cannot be reached
// as configured.
type failing struct{ err error }

func (f failing) Do(*http.Request) (*http.Response, error) { return nil, f.err }
