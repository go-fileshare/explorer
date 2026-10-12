// SPDX-License-Identifier: BSD-3-Clause

//go:build js && wasm

// The browser build: served by go-fileshare/portal at /explorer/, it reaches
// the servers' files through the portal's /dav/, which holds the tokens.
package main

import (
	"context"
	"errors"
	"syscall/js"

	"github.com/go-widgets/mvvm"
	"github.com/go-widgets/window"

	"github.com/go-fileshare/portal/client"

	"github.com/go-fileshare/explorer/internal/dav"
)

func main() {
	origin := js.Global().Get("location").Get("origin").String()
	p := client.New(origin, nil, client.WebLocks(client.RefreshLock))
	w, err := window.Open(window.Config{Title: "fileshare explorer", Canvas: "screen"})
	if err != nil {
		println("explorer:", err.Error())
		return
	}
	var wake func()
	if r, ok := w.(window.Repainter); ok {
		wake = r.Repaint
	}
	q := mvvm.NewQueue(wake)
	vm := newViewModel(context.Background(), portalBackend{p}, browserHost{}, q)
	v := newView(vm, nil, q, nil)
	expose(vm)
	vm.Start()
	_ = w.Run(v.root)
}

// browserHost is the page.
type browserHost struct{}

func (browserHost) Navigate(url string) { js.Global().Get("location").Call("assign", url) }

// Download follows a link to e: the browser fetches it, as the attachment
// the portal makes it, and writes it to disk as it comes.
func (browserHost) Download(c *dav.Client, e dav.Entry) {
	doc := js.Global().Get("document")
	a := doc.Call("createElement", "a")
	a.Set("href", c.DownloadURL(e.Path))
	a.Set("download", "")
	doc.Get("body").Call("appendChild", a)
	a.Call("click")
	a.Call("remove")
}

// PickFiles opens the browser's file picker. It must run inside the click
// that asked for it -- a browser opens a picker only on a user gesture --
// which it does: commands run in the canvas's mousedown handler.
func (browserHost) PickFiles(got func([]upload)) {
	doc := js.Global().Get("document")
	in := doc.Call("createElement", "input")
	in.Set("type", "file")
	in.Set("multiple", true)
	var onchange js.Func
	onchange = js.FuncOf(func(js.Value, []js.Value) any {
		files := in.Get("files")
		var ups []upload
		for i := 0; i < files.Length(); i++ {
			f := files.Index(i)
			ups = append(ups, upload{Name: f.Get("name").String(), Src: jsFile{f}})
		}
		onchange.Release()
		in.Call("remove")
		got(ups)
		return nil
	})
	in.Call("addEventListener", "change", onchange)
	in.Get("style").Set("display", "none")
	doc.Get("body").Call("appendChild", in)
	in.Call("click")
}

// jsFile is a browser File, read a piece at a time: never whole.
type jsFile struct{ f js.Value }

func (j jsFile) Size() int64 { return int64(j.f.Get("size").Float()) }

// Fingerprint is the file's name, size and time (File.lastModified): an
// upload that failed is resumed by the same file only.
func (j jsFile) Fingerprint() string {
	return dav.Fingerprint(j.f.Get("name").String(), j.Size(), int64(j.f.Get("lastModified").Float()))
}

func (j jsFile) ReadPiece(ctx context.Context, off, n int64) ([]byte, error) {
	type result struct {
		b   []byte
		err error
	}
	ch := make(chan result, 1)
	var then, catch js.Func
	then = js.FuncOf(func(_ js.Value, a []js.Value) any {
		u8 := js.Global().Get("Uint8Array").New(a[0])
		b := make([]byte, u8.Length())
		js.CopyBytesToGo(b, u8)
		ch <- result{b: b}
		return nil
	})
	catch = js.FuncOf(func(_ js.Value, a []js.Value) any {
		ch <- result{err: errors.New("the browser could not read the file: " + a[0].Call("toString").String())}
		return nil
	})
	defer then.Release()
	defer catch.Release()
	j.f.Call("slice", float64(off), float64(off+n)).Call("arrayBuffer").Call("then", then).Call("catch", catch)
	select {
	case r := <-ch:
		return r.b, r.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
