// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// memFS is a disk for the picker's tests: folders and files by path.
type memFS map[string][]localEntry

func (m memFS) List(dir string) ([]localEntry, error) {
	es, ok := m[dir]
	if !ok {
		return nil, errors.New("no folder " + dir)
	}
	return append([]localEntry(nil), es...), nil
}

func (m memFS) Source(path string) (upload, error) {
	if strings.HasSuffix(path, "locked.txt") {
		return upload{}, errors.New("permission denied")
	}
	return upload{Name: filepath.Base(path), Src: bytesSource("x")}, nil
}

func names(p *picker) string {
	var out []string
	for _, e := range p.Entries.Slice() {
		out = append(out, pickerLine(e))
	}
	return strings.Join(out, ",")
}

func TestPicker(t *testing.T) {
	root := filepath.FromSlash("/home/alice")
	docs := filepath.Join(root, "docs")
	fs := memFS{
		root: {{Name: "zeta.txt", Size: 2048}, {Name: ".hidden"}, {Name: "docs", Dir: true}, {Name: "Alpha.txt", Size: 1}, {Name: "locked.txt"}},
		docs: {{Name: "report.pdf", Size: 10}},
	}
	p := newPicker(fs, root)
	var got []upload
	calls := 0
	p.Show(func(u []upload) { got, calls = u, calls+1 })
	sep := string(filepath.Separator)
	// Folders first, then by name whatever the case; hidden left out.
	if !p.Visible.Get() || names(p) != "docs"+sep+",Alpha.txt  1 B,locked.txt  0 B,zeta.txt  2.0 KiB" || p.Sel.Get() != 0 {
		t.Fatalf("%v %q %d", p.Visible.Get(), names(p), p.Sel.Get())
	}
	// Into a folder, and back up: the folder we came from is selected.
	p.Open.Execute()
	if p.Dir.Get() != docs || names(p) != "report.pdf  10 B" {
		t.Fatalf("%s %q", p.Dir.Get(), names(p))
	}
	p.Up.Execute()
	if e, _ := p.entry(); p.Dir.Get() != root || e.Name != "docs" {
		t.Fatalf("%s, selected %q", p.Dir.Get(), e.Name)
	}
	// A file that cannot be read says so and stays open.
	p.Sel.Set(2)
	p.Open.Execute()
	if !p.Visible.Get() || p.Status.Get() != "permission denied" || calls != 0 {
		t.Fatalf("%v %q %d", p.Visible.Get(), p.Status.Get(), calls)
	}
	// Choosing a file closes the picker and hands it over, once.
	p.Sel.Set(1)
	p.Open.Execute()
	if p.Visible.Get() || calls != 1 || len(got) != 1 || got[0].Name != "Alpha.txt" {
		t.Fatalf("%v %d %+v", p.Visible.Get(), calls, got)
	}
	// Cancel hands over nothing; a folder that cannot be listed is said.
	p.Show(func(u []upload) { got, calls = u, calls+1 })
	p.Dir.Set(filepath.Join(root, "gone"))
	p.list("")
	if p.Entries.Len() != 0 || !strings.Contains(p.Status.Get(), "no folder") || p.Open.CanExecute() {
		t.Fatalf("%q %d", p.Status.Get(), p.Entries.Len())
	}
	p.Cancel.Execute()
	if p.Visible.Get() || calls != 2 || got != nil {
		t.Fatalf("%v %d %+v", p.Visible.Get(), calls, got)
	}
	// At the top of the disk there is no Up.
	p.Dir.Set(filepath.VolumeName(root) + string(filepath.Separator))
	if p.Up.CanExecute() {
		t.Fatal("Up at the root")
	}
}

// The native picker, through the explorer: Upload opens it in place of the
// files, choosing a file uploads it, and the files come back.
func TestPickerUploads(t *testing.T) {
	f := newFixture(t)
	root := filepath.FromSlash("/home/alice")
	pk := newPicker(memFS{root: {{Name: "note.txt", Size: 1}}}, root)
	f.vm.host = pickHost{f.h, pk}
	v := newView(f.vm, pk, f.q, nil)
	f.into("/files/")
	layout(t, v)
	click(v, v.upload)
	if !pk.Visible.Get() || v.pages.Visible().Get() != "pick" {
		t.Fatal("the picker is not shown")
	}
	layout(t, v)
	if strings.Join(v.local.Items, "|") != "note.txt  1 B" {
		t.Fatalf("%q", v.local.Items)
	}
	click(v, v.localOpen)
	f.settle()
	if v.pages.Visible().Get() != "files" || !strings.Contains(f.names(), "note.txt") {
		t.Fatalf("%s: %s, %q", v.pages.Visible().Get(), f.names(), f.vm.Status.Get())
	}
}

// pickHost is the fake host with the native picker.
type pickHost struct {
	*fakeHost
	pk *picker
}

func (h pickHost) PickFiles(got func([]upload)) { h.pk.Show(got) }
