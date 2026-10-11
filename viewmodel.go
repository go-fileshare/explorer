// SPDX-License-Identifier: BSD-3-Clause
//
// The ViewModel: all of the explorer's state, as go-widgets/mvvm primitives,
// and never a widget. Every call to a server runs on its own goroutine and
// comes back through an mvvm.Queue, applied on the UI goroutine.

package main

import (
	"context"
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/go-widgets/mvvm"

	"github.com/go-fileshare/portal/client"

	"github.com/go-fileshare/explorer/internal/dav"
)

// host is what the explorer needs of where it runs: the browser, or a test.
type host interface {
	// Download has the browser fetch url -- a navigation, which the portal
	// answers as an attachment -- so the file streams to disk and never
	// through this program's memory.
	Download(url string)
	// PickFiles asks the person for files to upload, and hands them over.
	PickFiles(got func([]upload))
	// Navigate leaves the explorer: to log in, or to log out at the
	// provider.
	Navigate(url string)
}

// An upload is one picked file.
type upload struct {
	Name string
	Src  dav.Source
}

// pieceMax is the portal's bound on one PUT: an upload piece.
const pieceMax = 16 << 20

type viewModel struct {
	portal *client.Portal
	host   host
	q      *mvvm.Queue
	ctx    context.Context

	User, Status *mvvm.Observable[string]
	Busy         *mvvm.Observable[bool]

	Servers   *mvvm.ObservableList[client.Server]
	ServerSel *mvvm.Observable[int]
	// Dir is the folder shown, from the server's WebDAV root: "/" lists
	// the shares, "/photos/2026/" a folder in one.
	Dir      *mvvm.Observable[string]
	Entries  *mvvm.ObservableList[dav.Entry]
	EntrySel *mvvm.Observable[int]
	// Name is the one text field: a new folder's name, or a selected
	// entry's new one.
	Name *mvvm.Observable[string]

	Open, Up, Reload, NewFolder, Rename, Duplicate, Delete, Download, Upload, Logout *mvvm.Command

	pending    int
	generation int
	armed      string
	clients    map[string]*dav.Client
}

func newViewModel(ctx context.Context, p *client.Portal, h host, q *mvvm.Queue) *viewModel {
	vm := &viewModel{portal: p, host: h, q: q, ctx: ctx,
		User: mvvm.NewObservable(""), Status: mvvm.NewObservable("Loading..."), Busy: mvvm.NewObservable(false),
		Servers: mvvm.NewObservableList[client.Server](), ServerSel: mvvm.NewObservable(-1),
		Dir: mvvm.NewObservable("/"), Entries: mvvm.NewObservableList[dav.Entry](), EntrySel: mvvm.NewObservable(-1),
		Name: mvvm.NewObservable(""), clients: map[string]*dav.Client{},
	}
	server := func() bool { _, ok := vm.server(); return ok }
	entry := func() bool { _, ok := vm.entry(); return ok }
	inShare := func() bool { return server() && vm.Dir.Get() != "/" }
	named := func() bool { return strings.TrimSpace(vm.Name.Get()) != "" }
	vm.Open = mvvm.NewCommand(vm.open, entry)
	vm.Up = mvvm.NewCommand(vm.up, func() bool { return server() && vm.Dir.Get() != "/" })
	vm.Reload = mvvm.NewCommand(vm.load, server)
	vm.NewFolder = mvvm.NewCommand(vm.newFolder, func() bool { return inShare() && named() })
	vm.Rename = mvvm.NewCommand(vm.rename, func() bool { return inShare() && entry() && named() })
	vm.Duplicate = mvvm.NewCommand(vm.duplicate, func() bool { return inShare() && entry() && named() })
	vm.Delete = mvvm.NewCommand(vm.delete, func() bool { return inShare() && entry() })
	vm.Download = mvvm.NewCommand(vm.download, func() bool { e, ok := vm.entry(); return ok && !e.Dir })
	vm.Upload = mvvm.NewCommand(vm.pick, inShare)
	vm.Logout = mvvm.NewCommand(vm.logout, nil)
	for _, c := range []*mvvm.Command{vm.Open, vm.Up, vm.Reload, vm.NewFolder, vm.Rename, vm.Duplicate, vm.Delete, vm.Download, vm.Upload} {
		mvvm.BindCanExecute(c, vm.ServerSel, vm.Servers, vm.Dir, vm.EntrySel, vm.Entries, vm.Name)
	}
	vm.ServerSel.SubscribeChanged(func() { vm.Dir.Set("/"); vm.load() })
	vm.EntrySel.SubscribeChanged(func() {
		vm.armed = ""
		if e, ok := vm.entry(); ok {
			vm.Name.Set(e.Name)
		}
	})
	return vm
}

// async runs fn on its own goroutine and applies its result on the UI
// goroutine; a result about a server or folder no longer shown is dropped.
func async[T any](vm *viewModel, what string, fn func(ctx context.Context) (T, error), apply func(T)) {
	vm.pending++
	vm.Busy.Set(true)
	gen := vm.generation
	go func() {
		v, err := fn(vm.ctx)
		vm.q.Post(func() {
			vm.pending--
			vm.Busy.Set(vm.pending > 0)
			if gen != vm.generation {
				return
			}
			if err != nil {
				vm.fail(what, err)
				return
			}
			apply(v)
		})
	}()
}

func (vm *viewModel) fail(what string, err error) {
	if errors.Is(err, client.ErrNotLoggedIn) {
		vm.Status.Set("You are not logged in, or your session has ended.")
		vm.host.Navigate(vm.portal.LoginURL("/explorer/"))
		return
	}
	if dav.Exists(err) {
		vm.Status.Set(what + ": something with that name is there already")
		return
	}
	var de *dav.Error
	if errors.As(err, &de) {
		switch de.Status {
		case 401:
			vm.Status.Set(what + ": the server refused this login")
		case 403:
			vm.Status.Set(what + ": not allowed")
		case 404:
			vm.Status.Set(what + ": not found")
		case 507:
			vm.Status.Set(what + ": the share is full")
		default:
			vm.Status.Set(fmt.Sprintf("%s: the server answered %d", what, de.Status))
		}
		return
	}
	vm.Status.Set(what + ": " + err.Error())
}

// Start asks who is logged in, and lists the servers whose files the portal
// passes.
func (vm *viewModel) Start() {
	async(vm, "Session", vm.portal.Session, func(s *client.Session) {
		vm.User.Set(s.Who())
		vm.Servers.Clear()
		for _, sv := range s.Servers {
			if sv.DAV && sv.Reachable {
				vm.Servers.Append(sv)
			}
		}
		vm.Status.Set("")
		if vm.Servers.Len() == 0 {
			vm.Status.Set("No server's files are reachable from this login.")
			return
		}
		vm.ServerSel.Set(0)
	})
}

func (vm *viewModel) server() (client.Server, bool) {
	i := vm.ServerSel.Get()
	if i < 0 || i >= vm.Servers.Len() {
		return client.Server{}, false
	}
	return vm.Servers.At(i), true
}

func (vm *viewModel) entry() (dav.Entry, bool) {
	i := vm.EntrySel.Get()
	if i < 0 || i >= vm.Entries.Len() {
		return dav.Entry{}, false
	}
	return vm.Entries.At(i), true
}

// dav is the selected server's WebDAV, through the portal.
func (vm *viewModel) dav() *dav.Client {
	s, _ := vm.server()
	c, ok := vm.clients[s.ID]
	if !ok {
		c = dav.New(vm.portal.Base()+"/dav/"+s.ID, vm.portal.Doer(s.ID, pieceMax))
		vm.clients[s.ID] = c
	}
	return c
}

// load lists the folder shown.
func (vm *viewModel) load() { vm.loadKeep("") }

func (vm *viewModel) loadKeep(keep string) {
	vm.generation++
	vm.armed = ""
	if _, ok := vm.server(); !ok {
		vm.setEntries(nil, "")
		return
	}
	c, dir := vm.dav(), vm.Dir.Get()
	async(vm, "List "+dir, func(ctx context.Context) ([]dav.Entry, error) {
		return c.List(ctx, dir)
	}, func(es []dav.Entry) {
		if dir != "/" {
			// Names a part file has while an upload is under way.
			es = slices.DeleteFunc(es, func(e dav.Entry) bool { return strings.HasSuffix(e.Name, ".part") })
		}
		vm.setEntries(es, keep)
	})
}

func (vm *viewModel) setEntries(es []dav.Entry, keep string) {
	vm.Entries.Clear()
	vm.Entries.Append(es...)
	sel := slices.IndexFunc(es, func(e dav.Entry) bool { return e.Name == keep })
	if sel < 0 && len(es) > 0 {
		sel = 0
	}
	vm.EntrySel.Set(-1) // so that setting sel always notifies
	vm.EntrySel.Set(sel)
	if len(es) == 0 {
		vm.Name.Set("")
	}
}

// open goes into a folder, or downloads a file.
func (vm *viewModel) open() {
	e, _ := vm.entry()
	if !e.Dir {
		vm.download()
		return
	}
	vm.Dir.Set(e.Path)
	vm.load()
}

func (vm *viewModel) up() {
	d := strings.TrimSuffix(vm.Dir.Get(), "/")
	parent := path.Dir(d)
	if parent != "/" {
		parent += "/"
	}
	vm.Dir.Set(parent)
	vm.loadKeep(path.Base(d))
}

// child is the path of name in the folder shown, refusing a name that is not
// one: a slash would make it a path, and . or .. somewhere else.
func (vm *viewModel) child(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\\x00") {
		return "", fmt.Errorf("%q is not a name", name)
	}
	return vm.Dir.Get() + name, nil
}

func (vm *viewModel) newFolder() {
	p, err := vm.child(vm.Name.Get())
	if err != nil {
		vm.Status.Set("New folder: " + err.Error())
		return
	}
	c, name := vm.dav(), path.Base(p)
	async(vm, "New folder "+name, func(ctx context.Context) (struct{}, error) {
		return struct{}{}, c.Mkdir(ctx, p)
	}, func(struct{}) {
		vm.Status.Set("Folder " + name + " made.")
		vm.loadKeep(name)
	})
}

func (vm *viewModel) transfer(verb string, copy bool) {
	e, _ := vm.entry()
	to, err := vm.child(vm.Name.Get())
	if err != nil {
		vm.Status.Set(verb + ": " + err.Error())
		return
	}
	if e.Dir {
		to += "/"
	}
	if to == e.Path {
		vm.Status.Set(verb + ": give it another name first")
		return
	}
	c, name := vm.dav(), path.Base(strings.TrimSuffix(to, "/"))
	async(vm, verb+" "+e.Name, func(ctx context.Context) (struct{}, error) {
		if copy {
			return struct{}{}, c.Copy(ctx, e.Path, to)
		}
		return struct{}{}, c.Move(ctx, e.Path, to)
	}, func(struct{}) {
		if copy {
			vm.Status.Set(name + " is a copy of " + e.Name + ".")
		} else {
			vm.Status.Set(e.Name + " is now " + name + ".")
		}
		vm.loadKeep(name)
	})
}

func (vm *viewModel) rename()    { vm.transfer("Rename", false) }
func (vm *viewModel) duplicate() { vm.transfer("Duplicate", true) }

// delete is two presses, and says what will go.
func (vm *viewModel) delete() {
	e, _ := vm.entry()
	if vm.armed != e.Path {
		vm.armed = e.Path
		what := "file " + e.Name
		if e.Dir {
			what = "folder " + e.Name + " and everything in it"
		}
		vm.Status.Set("Press Delete again to delete the " + what + ".")
		return
	}
	vm.armed = ""
	c := vm.dav()
	async(vm, "Delete "+e.Name, func(ctx context.Context) (struct{}, error) {
		return struct{}{}, c.Delete(ctx, e.Path)
	}, func(struct{}) {
		vm.Status.Set(e.Name + " deleted.")
		vm.load()
	})
}

// download buys the token first -- a navigation cannot refresh and try
// again -- then has the browser fetch the file.
func (vm *viewModel) download() {
	e, _ := vm.entry()
	s, _ := vm.server()
	c := vm.dav()
	async(vm, "Download "+e.Name, func(ctx context.Context) (struct{}, error) {
		return struct{}{}, vm.portal.EnsureToken(ctx, s.ID)
	}, func(struct{}) {
		// Started, not done: a browser does not tell the page when its
		// download ends. The native host says so itself when it does.
		vm.Status.Set(e.Name + ": download started.")
		vm.host.Download(c.DownloadURL(e.Path))
	})
}

func (vm *viewModel) pick() {
	vm.host.PickFiles(func(ups []upload) {
		// PickFiles may answer on any goroutine (a browser callback).
		vm.q.Post(func() { vm.uploadAll(ups) })
	})
}

// uploadAll sends the picked files one after the other, in pieces, showing
// how far each has gone. A name already there is refused, never replaced.
func (vm *viewModel) uploadAll(ups []upload) {
	if len(ups) == 0 {
		return
	}
	c := vm.dav()
	// The paths are worked out here, on the UI goroutine: the folder shown
	// is an observable, which the upload's goroutine must not read.
	paths := make([]string, len(ups))
	for i, u := range ups {
		p, err := vm.child(path.Base(u.Name))
		if err != nil {
			vm.Status.Set("Upload: " + err.Error())
			return
		}
		paths[i] = p
	}
	start := time.Now()
	async(vm, "Upload", func(ctx context.Context) (int, error) {
		for i, u := range ups {
			p := paths[i]
			size := u.Src.Size()
			err := c.Upload(ctx, p, u.Src, false, func(sent int64) {
				msg := fmt.Sprintf("Uploading %s (%d of %d): %s of %s", u.Name, i+1, len(ups), bytesString(sent), bytesString(size))
				vm.q.Post(func() { vm.Status.Set(msg) })
			})
			if err != nil {
				return i, fmt.Errorf("%s: %w", u.Name, err)
			}
		}
		return len(ups), nil
	}, func(n int) {
		vm.Status.Set(fmt.Sprintf("%d uploaded in %s.", n, time.Since(start).Round(time.Second)))
		vm.loadKeep(ups[len(ups)-1].Name)
	})
}

func (vm *viewModel) logout() {
	async(vm, "Log out", vm.portal.Logout, func(end string) {
		vm.Status.Set("Logged out.")
		if end == "" {
			end = vm.portal.LoginURL("/explorer/")
		}
		vm.host.Navigate(end)
	})
}

// entryLine is how an entry is listed.
func entryLine(e dav.Entry) string {
	if e.Dir {
		return e.Name + "/"
	}
	line := e.Name + "  " + bytesString(e.Size)
	if !e.Modified.IsZero() {
		line += "  " + e.Modified.Local().Format("2006-01-02 15:04")
	}
	return line
}

// bytesString is n in binary units.
func bytesString(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit && exp < 5; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
