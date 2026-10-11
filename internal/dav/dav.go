// SPDX-License-Identifier: BSD-3-Clause

// Package dav is the explorer's WebDAV client: what browsing, downloading and
// uploading a share's files need of RFC 4918, and nothing else -- no locks,
// no properties beyond the four a listing shows.
//
// It talks through any HTTP client (Doer): in a browser, the portal's
// (go-fileshare/portal/client), which adds the portal's header and keeps the
// token fresh; in a test, a plain one.
package dav

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Doer sends a request: *http.Client and the portal client's Doer are both.
type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

// Client is the WebDAV of one server, rooted at base -- the portal's
// /dav/<server>, or a server's own URL.
type Client struct {
	base string
	do   Doer
	// Piece is the size of one upload piece. Default 8 MiB: the portal
	// takes 16 MiB at most per PUT, and a smaller piece is a finer progress
	// bar and a smaller retry.
	Piece int64
}

// New is a client of the WebDAV at base.
func New(base string, do Doer) *Client {
	return &Client{base: strings.TrimSuffix(base, "/"), do: do, Piece: 8 << 20}
}

// An Entry is one resource of a listing.
type Entry struct {
	Name     string
	Path     string // from the root of the server's WebDAV, with a leading /; a collection's ends in /
	Dir      bool
	Size     int64
	Modified time.Time
}

// Error is a status a request was refused with, and what the server said.
type Error struct {
	Op, Path string
	Status   int
	Message  string
}

func (e *Error) Error() string {
	msg := strings.TrimSpace(e.Message)
	if msg == "" {
		msg = http.StatusText(e.Status)
	}
	return fmt.Sprintf("%s %s: %d %s", e.Op, e.Path, e.Status, msg)
}

// Exists says whether err is a MOVE or COPY refused because something is
// at the destination already (412 with Overwrite: F, RFC 4918 §10.6).
func Exists(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Status == http.StatusPreconditionFailed
}

// url is the request URL of a server path: every segment escaped.
func (c *Client) url(p string) string {
	segs := strings.Split(p, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return c.base + strings.Join(segs, "/")
}

// DownloadURL is where a browser fetches p: a navigation, which the portal
// answers as an attachment.
func (c *Client) DownloadURL(p string) string { return c.url(p) }

// Get is the file p, to be read as it arrives and closed. A collection is
// not a file: p must not end in /.
func (c *Client) Get(ctx context.Context, p string) (io.ReadCloser, error) {
	if strings.HasSuffix(p, "/") {
		return nil, fmt.Errorf("download %s: a folder is not a file", p)
	}
	res, err := c.send(ctx, "download", http.MethodGet, p, nil)
	if err != nil {
		return nil, err
	}
	if res.StatusCode != http.StatusOK {
		return nil, expect("download", p, res)
	}
	return res.Body, nil
}

func (c *Client) send(ctx context.Context, op, method, p string, body []byte, hdr ...string) (*http.Response, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.url(p), rd)
	if err != nil {
		return nil, err
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	res, err := c.do.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", op, p, err)
	}
	return res, nil
}

// expect reads res and returns an *Error unless its status is one of ok.
func expect(op, p string, res *http.Response, ok ...int) error {
	defer res.Body.Close()
	if slices.Contains(ok, res.StatusCode) {
		_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 1<<20))
		return nil
	}
	msg, _ := io.ReadAll(io.LimitReader(res.Body, 4<<10))
	return &Error{Op: op, Path: p, Status: res.StatusCode, Message: string(msg)}
}

const propfindBody = `<?xml version="1.0" encoding="utf-8"?>
<D:propfind xmlns:D="DAV:"><D:prop>
<D:resourcetype/><D:getcontentlength/><D:getlastmodified/><D:displayname/>
</D:prop></D:propfind>`

type multistatus struct {
	Responses []struct {
		Href     string `xml:"href"`
		Propstat []struct {
			Status string `xml:"status"`
			Prop   struct {
				ResourceType struct {
					Collection *struct{} `xml:"collection"`
				} `xml:"resourcetype"`
				Length   string `xml:"getcontentlength"`
				Modified string `xml:"getlastmodified"`
			} `xml:"prop"`
		} `xml:"propstat"`
	} `xml:"response"`
}

// List is what the collection dir holds (PROPFIND, Depth 1), collections
// first, then by name; dir itself is not in it.
func (c *Client) List(ctx context.Context, dir string) ([]Entry, error) {
	if !strings.HasSuffix(dir, "/") {
		dir += "/"
	}
	res, err := c.send(ctx, "list", "PROPFIND", dir, []byte(propfindBody),
		"Depth", "1", "Content-Type", "application/xml; charset=utf-8")
	if err != nil {
		return nil, err
	}
	if res.StatusCode != http.StatusMultiStatus {
		return nil, expect("list", dir, res)
	}
	defer res.Body.Close()
	var ms multistatus
	if err := xml.NewDecoder(io.LimitReader(res.Body, 16<<20)).Decode(&ms); err != nil {
		return nil, fmt.Errorf("list %s: %w", dir, err)
	}
	var out []Entry
	for _, r := range ms.Responses {
		p, err := hrefPath(r.Href)
		if err != nil {
			continue
		}
		if strings.TrimSuffix(p, "/") == strings.TrimSuffix(dir, "/") {
			continue // the collection itself
		}
		e := Entry{Path: p}
		for _, ps := range r.Propstat {
			if !strings.Contains(ps.Status, " 200 ") {
				continue
			}
			if ps.Prop.ResourceType.Collection != nil {
				e.Dir = true
			}
			if n, err := strconv.ParseInt(strings.TrimSpace(ps.Prop.Length), 10, 64); err == nil {
				e.Size = n
			}
			if t, err := http.ParseTime(strings.TrimSpace(ps.Prop.Modified)); err == nil {
				e.Modified = t
			}
		}
		if e.Dir && !strings.HasSuffix(e.Path, "/") {
			e.Path += "/"
		}
		e.Name = path.Base(strings.TrimSuffix(e.Path, "/"))
		out = append(out, e)
	}
	slices.SortFunc(out, func(a, b Entry) int {
		if a.Dir != b.Dir {
			if a.Dir {
				return -1
			}
			return 1
		}
		return strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name))
	})
	return out, nil
}

// hrefPath is the server path an href names: an absolute URL's path, or a
// path, unescaped.
func hrefPath(href string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(href))
	if err != nil {
		return "", err
	}
	return u.Path, nil
}

// Mkdir makes the collection p.
func (c *Client) Mkdir(ctx context.Context, p string) error {
	res, err := c.send(ctx, "new folder", "MKCOL", strings.TrimSuffix(p, "/")+"/", nil)
	if err != nil {
		return err
	}
	return expect("new folder", p, res, http.StatusCreated)
}

// Delete removes p, a collection with everything in it.
func (c *Client) Delete(ctx context.Context, p string) error {
	res, err := c.send(ctx, "delete", http.MethodDelete, p, nil)
	if err != nil {
		return err
	}
	return expect("delete", p, res, http.StatusNoContent, http.StatusOK)
}

// Move renames from to to; Copy copies it there, on the server. Neither
// replaces what is there already: Overwrite is F (RFC 4918 §10.6 makes T the
// default, which would silently destroy a file a rename landed on), and an
// existing destination is an error Exists recognises. A 207 is a move or copy
// that failed in part, and is an error too.
func (c *Client) Move(ctx context.Context, from, to string) error {
	return c.transfer(ctx, "MOVE", from, to)
}

// Copy is Move's twin; see Move.
func (c *Client) Copy(ctx context.Context, from, to string) error {
	return c.transfer(ctx, "COPY", from, to)
}

func (c *Client) transfer(ctx context.Context, method, from, to string) error {
	op := strings.ToLower(method)
	res, err := c.send(ctx, op, method, from, nil, "Destination", c.url(to), "Overwrite", "F", "Depth", "infinity")
	if err != nil {
		return err
	}
	return expect(op, from, res, http.StatusCreated, http.StatusNoContent)
}

// A Source is a file to upload: the browser's File, a local file.
type Source interface {
	Size() int64
	// ReadPiece reads the n bytes at off.
	ReadPiece(ctx context.Context, off, n int64) ([]byte, error)
}

// Upload writes src at p, in pieces: an empty PUT of p.part, each piece
// appended where the last ended (go-filesystems/webdav v0.6.0), then a MOVE
// to p -- so nobody sees half a file under its name, and no request holds the
// whole file. progress, when not nil, is told the bytes sent so far. A
// failure leaves p as it was, and tries to remove the part file.
func (c *Client) Upload(ctx context.Context, p string, src Source, overwrite bool, progress func(sent int64)) (err error) {
	part := p + ".part"
	res, err := c.send(ctx, "upload", http.MethodPut, part, []byte{})
	if err != nil {
		return err
	}
	if err := expect("upload", part, res, http.StatusCreated, http.StatusNoContent); err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = c.Delete(context.WithoutCancel(ctx), part)
		}
	}()
	size := src.Size()
	for off := int64(0); off < size; {
		n := min(c.Piece, size-off)
		b, err := src.ReadPiece(ctx, off, n)
		if err != nil {
			return fmt.Errorf("upload %s: reading at %d: %w", p, off, err)
		}
		if int64(len(b)) != n {
			return fmt.Errorf("upload %s: read %d bytes at %d, want %d", p, len(b), off, n)
		}
		rng := fmt.Sprintf("bytes %d-%d/%d", off, off+n-1, size)
		res, err := c.send(ctx, "upload", http.MethodPut, part, b, "Content-Range", rng)
		if err != nil {
			return err
		}
		if err := expect("upload", part, res, http.StatusNoContent, http.StatusOK, http.StatusCreated); err != nil {
			return err
		}
		off += n
		if progress != nil {
			progress(off)
		}
	}
	ow := "F"
	if overwrite {
		ow = "T"
	}
	res, err = c.send(ctx, "upload", "MOVE", part, nil, "Destination", c.url(p), "Overwrite", ow)
	if err != nil {
		return err
	}
	return expect("upload", p, res, http.StatusCreated, http.StatusNoContent)
}
