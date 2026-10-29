// SPDX-License-Identifier: BSD-3-Clause

//go:build js && wasm

package main

// The bridge build: a page that holds the files itself -- the go-filesystems
// playground, whose engine serves an opened disk image's WebDAV inside the
// tab -- embeds the explorer in a same-origin iframe and hands it a bridge in
// place of the portal:
//
//	parent.fileshareExplorerBridge = {
//	  label: "disk.img",                     // what the one server is called
//	  request(method, path, headers, body)   // -> Promise<{status, headers, body}>
//	  download(path, name),                  // the page saves the file
//	  close(),                               // "Log out": leave the explorer
//	  state(json),                           // optional: what the explorer shows
//	}
//
// headers are [name, value] pairs and body a Uint8Array or null, both ways.
// There is no login and no token: the files never leave the page.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"syscall/js"

	"github.com/go-fileshare/portal/client"

	"github.com/go-fileshare/explorer/internal/dav"
)

// bridgeServer is the ID of the bridge's one server.
const bridgeServer = "files"

// findBridge returns the parent page's bridge, or ok false. Reading a
// property of a parent of another origin throws, which syscall/js turns into
// a panic: that parent has no bridge for us.
func findBridge() (b js.Value, ok bool) {
	defer func() {
		if recover() != nil {
			b, ok = js.Undefined(), false
		}
	}()
	self := js.Global()
	parent := self.Get("parent")
	if parent.IsUndefined() || parent.IsNull() || parent.Equal(self) {
		return js.Undefined(), false
	}
	b = parent.Get("fileshareExplorerBridge")
	if b.Type() != js.TypeObject || b.Get("request").Type() != js.TypeFunction {
		return js.Undefined(), false
	}
	return b, true
}

// bridgeBackend is the page's one server, reached through the bridge.
type bridgeBackend struct{ b js.Value }

func (k bridgeBackend) label() string {
	if l := k.b.Get("label"); l.Type() == js.TypeString && l.String() != "" {
		return l.String()
	}
	return "files"
}

func (k bridgeBackend) Session(context.Context) (*client.Session, error) {
	l := k.label()
	return &client.Session{Name: l, Servers: []client.Server{{ID: bridgeServer, Label: l, Reachable: true, DAV: true}}}, nil
}

func (k bridgeBackend) DAV(string) *dav.Client {
	c := dav.New("", bridgeDoer{k.b})
	c.Piece = 4 << 20 // the bridge copies each piece twice, into JS and out
	return c
}

func (bridgeBackend) EnsureToken(context.Context, string) error { return nil }
func (bridgeBackend) Logout(context.Context) (string, error)    { return "", nil }
func (bridgeBackend) LoginURL(string) string                    { return "" }

// bridgeMax bounds a body either way: a piece, or a listing.
const bridgeMax = pieceMax

// bridgeDoer sends a request through the bridge.
type bridgeDoer struct{ b js.Value }

var errBridgeTooLarge = errors.New("the request is larger than the page takes")

func (d bridgeDoer) Do(req *http.Request) (*http.Response, error) {
	var body js.Value = js.Null()
	if req.Body != nil {
		b, err := io.ReadAll(io.LimitReader(req.Body, bridgeMax+1))
		req.Body.Close()
		if err != nil {
			return nil, err
		}
		if len(b) > bridgeMax {
			return nil, errBridgeTooLarge
		}
		body = js.Global().Get("Uint8Array").New(len(b))
		js.CopyBytesToJS(body, b)
	}
	hdr := js.Global().Get("Array").New()
	for k, vs := range req.Header {
		for _, v := range vs {
			hdr.Call("push", js.ValueOf([]any{k, v}))
		}
	}
	type result struct {
		res *http.Response
		err error
	}
	ch := make(chan result, 1)
	var then, catch js.Func
	then = js.FuncOf(func(_ js.Value, a []js.Value) any {
		res, err := bridgeResponse(a[0], req)
		ch <- result{res, err}
		return nil
	})
	catch = js.FuncOf(func(_ js.Value, a []js.Value) any {
		msg := "failed"
		if len(a) > 0 {
			msg = a[0].Call("toString").String()
		}
		ch <- result{err: errors.New("the page could not answer: " + msg)}
		return nil
	})
	defer then.Release()
	defer catch.Release()
	d.b.Call("request", req.Method, req.URL.EscapedPath(), hdr, body).Call("then", then).Call("catch", catch)
	select {
	case r := <-ch:
		return r.res, r.err
	case <-req.Context().Done():
		return nil, req.Context().Err()
	}
}

// bridgeResponse turns the page's answer into a response.
func bridgeResponse(v js.Value, req *http.Request) (res *http.Response, err error) {
	defer func() {
		if p := recover(); p != nil {
			res, err = nil, fmt.Errorf("the page answered something else: %v", p)
		}
	}()
	status := v.Get("status").Int()
	if status < 100 || status > 599 {
		return nil, fmt.Errorf("the page answered status %d", status)
	}
	h := http.Header{}
	if hs := v.Get("headers"); hs.Type() == js.TypeObject {
		for i := 0; i < hs.Length(); i++ {
			kv := hs.Index(i)
			h.Add(kv.Index(0).String(), kv.Index(1).String())
		}
	}
	var b []byte
	if bv := v.Get("body"); bv.Type() == js.TypeObject {
		n := bv.Get("length").Int()
		if n > bridgeMax {
			return nil, fmt.Errorf("the page answered %d bytes, more than %d", n, bridgeMax)
		}
		// The parent's Uint8Array is of another realm, which syscall/js does
		// not take for one: copy it into one of ours first.
		b = make([]byte, n)
		js.CopyBytesToGo(b, js.Global().Get("Uint8Array").New(bv))
	}
	return &http.Response{
		Status:        fmt.Sprintf("%d %s", status, http.StatusText(status)),
		StatusCode:    status,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        h,
		Body:          io.NopCloser(bytes.NewReader(b)),
		ContentLength: int64(len(b)),
		Request:       req,
	}, nil
}

// reportTo tells the page what the explorer shows, when the bridge has a
// state(json) function: {status, dir, entries}. The page may show it, and its
// tests read it (the explorer draws on a canvas, which they cannot).
func reportTo(b js.Value, vm *viewModel) {
	if b.Get("state").Type() != js.TypeFunction {
		return
	}
	report := func() {
		var entries []string
		for _, e := range vm.Entries.Slice() {
			entries = append(entries, e.Name)
		}
		j, _ := json.Marshal(map[string]any{"status": vm.Status.Get(), "dir": vm.Dir.Get(), "entries": entries})
		b.Call("state", string(j))
	}
	vm.Status.SubscribeChanged(report)
	vm.Dir.SubscribeChanged(report)
	vm.Entries.SubscribeChanged(report)
}

// bridgeHost is the explorer inside the page that holds the files.
type bridgeHost struct {
	browserHost
	b js.Value
}

// Download hands the file to the page, which reads and saves it.
func (h bridgeHost) Download(_ *dav.Client, e dav.Entry) {
	if f := h.b.Get("download"); f.Type() == js.TypeFunction {
		h.b.Call("download", e.Path, e.Name)
	}
}

// Navigate leaves the explorer: the page closes it.
func (h bridgeHost) Navigate(string) {
	if f := h.b.Get("close"); f.Type() == js.TypeFunction {
		h.b.Call("close")
	}
}
