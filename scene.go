// SPDX-License-Identifier: BSD-3-Clause
//
// The View: go-widgets/toolkit widgets bound to the ViewModel through
// go-widgets/mvvmtk; it writes no widget state of its own (go-widgets/
// mvvmlint). No build tag: scene_test.go builds, binds and draws it natively.

package main

import (
	"github.com/go-widgets/mvvm"
	"github.com/go-widgets/mvvmtk"
	"github.com/go-widgets/toolkit"

	"github.com/go-fileshare/portal/client"
)

// root drains the queue in SetBounds, which every window back-end calls
// before it draws: results from other goroutines are applied there, on the
// UI goroutine, and shown in the same frame.
type root struct {
	*toolkit.VBox
	q *mvvm.Queue
}

func (r *root) SetBounds(b toolkit.Rect) {
	r.q.Drain()
	r.VBox.SetBounds(b)
}

const (
	rowH    = 30
	titleH  = 22
	statusH = 24
	sideW   = 220
	buttonW = 104
)

type view struct {
	root                                 *root
	servers, entries                     *toolkit.ListBox
	user, dir, status                    *toolkit.Label
	name                                 *toolkit.Entry
	up, open, download, upload, del      *toolkit.Button
	newFolder, rename, duplicate, logout *toolkit.Button
	// The local picker's page, when there is one (natively).
	pages                         *toolkit.Stack
	local                         *toolkit.ListBox
	localDir                      *toolkit.Label
	localUp, localOpen, localStop *toolkit.Button
}

// newView builds the View of vm; pk, when not nil, is the local file picker,
// shown in place of the server's files while it is open.
func newView(vm *viewModel, pk *picker, q *mvvm.Queue, invalidate func()) *view {
	v := &view{
		servers: toolkit.NewListBox(nil), entries: toolkit.NewListBox(nil),
		user: toolkit.NewLabel(""), dir: toolkit.NewLabel(""), status: toolkit.NewLabel(""),
		name: toolkit.NewEntry(""),
		up:   toolkit.NewButton("Up", nil), open: toolkit.NewButton("Open", nil),
		download: toolkit.NewButton("Download", nil), upload: toolkit.NewButton("Upload...", nil),
		del: toolkit.NewButton("Delete", nil), newFolder: toolkit.NewButton("New folder", nil),
		rename: toolkit.NewButton("Rename", nil), duplicate: toolkit.NewButton("Duplicate", nil),
		logout: toolkit.NewButton("Log out", nil),
	}
	v.name.Placeholder = "a name: a new folder, or the selected item renamed"
	for _, l := range []*toolkit.Label{v.user, v.dir, v.status} {
		l.Ellipsis = true
	}

	mvvmtk.BindListItems(v.servers, vm.Servers, client.Server.Name, invalidate)
	mvvmtk.BindListSelection(v.servers, vm.ServerSel, invalidate)
	mvvmtk.BindListItems(v.entries, vm.Entries, entryLine, invalidate)
	mvvmtk.BindListSelection(v.entries, vm.EntrySel, invalidate)
	mvvmtk.BindLabel(v.user, vm.User, invalidate)
	mvvmtk.BindLabel(v.dir, vm.Dir, invalidate)
	mvvmtk.BindLabel(v.status, vm.Status, invalidate)
	mvvmtk.BindEntryText(v.name, vm.Name, invalidate)
	for b, c := range map[*toolkit.Button]*mvvm.Command{
		v.up: vm.Up, v.open: vm.Open, v.download: vm.Download, v.upload: vm.Upload, v.del: vm.Delete,
		v.newFolder: vm.NewFolder, v.rename: vm.Rename, v.duplicate: vm.Duplicate, v.logout: vm.Logout,
	} {
		mvvmtk.BindCommand(b, c, invalidate)
	}

	header := toolkit.NewHBox()
	header.Append(toolkit.NewLabel("fileshare explorer"))
	header.AddFixed(v.user, 240)
	header.AddFixed(v.logout, buttonW)

	left := toolkit.NewVBox()
	left.AddFixed(toolkit.NewLabel("Servers"), titleH)
	left.Append(v.servers)

	actions := toolkit.NewHBox()
	for _, b := range []*toolkit.Button{v.up, v.open, v.download, v.upload, v.del} {
		actions.AddFixed(b, buttonW)
	}
	naming := toolkit.NewHBox()
	naming.Append(v.name)
	for _, b := range []*toolkit.Button{v.newFolder, v.rename, v.duplicate} {
		naming.AddFixed(b, buttonW)
	}
	right := toolkit.NewVBox()
	right.AddFixed(v.dir, titleH)
	right.Append(v.entries)
	right.AddFixed(actions, rowH)
	right.AddFixed(naming, rowH)

	v.pages = toolkit.NewStack()
	v.pages.AddPage("files", right)
	if pk != nil {
		v.pages.AddPage("pick", v.pickerPage(pk, invalidate))
		pk.Visible.SubscribeChanged(func() {
			page := "files"
			if pk.Visible.Get() {
				page = "pick"
			}
			v.pages.SetVisible(page)
			if invalidate != nil {
				invalidate()
			}
		})
	}

	body := toolkit.NewHBox()
	body.AddFixed(left, sideW)
	body.Append(v.pages)

	col := toolkit.NewVBox()
	col.AddFixed(header, rowH)
	col.Append(body)
	col.AddFixed(v.status, statusH)
	v.root = &root{VBox: col, q: q}
	return v
}

// pickerPage is the local picker: a folder of this computer, and how to
// move through it or choose a file.
func (v *view) pickerPage(pk *picker, invalidate func()) toolkit.Widget {
	v.local, v.localDir = toolkit.NewListBox(nil), toolkit.NewLabel("")
	v.localDir.Ellipsis = true
	v.localUp, v.localOpen, v.localStop = toolkit.NewButton("Up", nil), toolkit.NewButton("Open", nil), toolkit.NewButton("Cancel", nil)
	mvvmtk.BindListItems(v.local, pk.Entries, pickerLine, invalidate)
	mvvmtk.BindListSelection(v.local, pk.Sel, invalidate)
	mvvmtk.BindLabel(v.localDir, pk.Status, invalidate)
	mvvmtk.BindCommand(v.localUp, pk.Up, invalidate)
	mvvmtk.BindCommand(v.localOpen, pk.Open, invalidate)
	mvvmtk.BindCommand(v.localStop, pk.Cancel, invalidate)
	actions := toolkit.NewHBox()
	for _, b := range []*toolkit.Button{v.localUp, v.localOpen, v.localStop} {
		actions.AddFixed(b, buttonW)
	}
	page := toolkit.NewVBox()
	page.AddFixed(toolkit.NewLabel("Upload a file from this computer: open it"), titleH)
	page.AddFixed(v.localDir, titleH)
	page.Append(v.local)
	page.AddFixed(actions, rowH)
	return page
}
