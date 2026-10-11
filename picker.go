// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"path/filepath"
	"slices"
	"strings"

	"github.com/go-widgets/mvvm"
)

// A localEntry is one item of a local folder.
type localEntry struct {
	Name string
	Dir  bool
	Size int64
}

// localFS is what the picker needs of this computer's disk: a folder's
// items, and a file to upload.
type localFS interface {
	List(dir string) ([]localEntry, error)
	Source(path string) (upload, error)
}

// picker chooses local files to upload, where the system has no picker the
// explorer can open (natively: go-widgets has no native file dialog). It is
// a ViewModel of its own; the View shows it in place of the server's files
// while Visible.
type picker struct {
	fs localFS

	Visible *mvvm.Observable[bool]
	Dir     *mvvm.Observable[string]
	Entries *mvvm.ObservableList[localEntry]
	Sel     *mvvm.Observable[int]
	Status  *mvvm.Observable[string]

	Open, Up, Cancel *mvvm.Command

	got func([]upload)
}

func newPicker(fs localFS, home string) *picker {
	p := &picker{fs: fs, Visible: mvvm.NewObservable(false), Dir: mvvm.NewObservable(home),
		Entries: mvvm.NewObservableList[localEntry](), Sel: mvvm.NewObservable(-1), Status: mvvm.NewObservable("")}
	p.Open = mvvm.NewCommand(p.open, func() bool { _, ok := p.entry(); return ok })
	p.Up = mvvm.NewCommand(p.up, func() bool { return filepath.Dir(p.Dir.Get()) != p.Dir.Get() })
	p.Cancel = mvvm.NewCommand(func() { p.done(nil) }, nil)
	mvvm.BindCanExecute(p.Open, p.Sel, p.Entries)
	mvvm.BindCanExecute(p.Up, p.Dir)
	return p
}

// Show opens the picker; got is told what was chosen -- nothing, when
// cancelled.
func (p *picker) Show(got func([]upload)) {
	p.got = got
	p.list("")
	p.Visible.Set(true)
}

func (p *picker) done(ups []upload) {
	p.Visible.Set(false)
	if got := p.got; got != nil {
		p.got = nil
		got(ups)
	}
}

func (p *picker) entry() (localEntry, bool) {
	i := p.Sel.Get()
	if i < 0 || i >= p.Entries.Len() {
		return localEntry{}, false
	}
	return p.Entries.At(i), true
}

// list shows the folder Dir, folders first, hidden items left out, keep
// selected.
func (p *picker) list(keep string) {
	es, err := p.fs.List(p.Dir.Get())
	if err != nil {
		p.Status.Set(err.Error())
		es = nil
	} else {
		p.Status.Set(p.Dir.Get())
	}
	es = slices.DeleteFunc(es, func(e localEntry) bool { return strings.HasPrefix(e.Name, ".") })
	slices.SortFunc(es, func(a, b localEntry) int {
		if a.Dir != b.Dir {
			if a.Dir {
				return -1
			}
			return 1
		}
		return strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name))
	})
	p.Entries.Clear()
	p.Entries.Append(es...)
	sel := slices.IndexFunc(es, func(e localEntry) bool { return e.Name == keep })
	if sel < 0 && len(es) > 0 {
		sel = 0
	}
	p.Sel.Set(-1)
	p.Sel.Set(sel)
}

// open goes into a folder, or chooses a file.
func (p *picker) open() {
	e, _ := p.entry()
	path := filepath.Join(p.Dir.Get(), e.Name)
	if e.Dir {
		p.Dir.Set(path)
		p.list("")
		return
	}
	u, err := p.fs.Source(path)
	if err != nil {
		p.Status.Set(err.Error())
		return
	}
	p.done([]upload{u})
}

func (p *picker) up() {
	from := filepath.Base(p.Dir.Get())
	p.Dir.Set(filepath.Dir(p.Dir.Get()))
	p.list(from)
}

// pickerLine is how a local item is listed.
func pickerLine(e localEntry) string {
	if e.Dir {
		return e.Name + string(filepath.Separator)
	}
	return e.Name + "  " + bytesString(e.Size)
}
