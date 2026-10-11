// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"strings"
	"testing"

	"github.com/go-widgets/painter"
	"github.com/go-widgets/toolkit"
)

const testW, testH = 1000, 640

// layout lays the tree out and draws it, as a window back-end does every
// frame -- which is also what drains the queue.
func layout(t *testing.T, v *view) []byte {
	t.Helper()
	buf := make([]byte, 4*testW*testH)
	v.root.SetBounds(toolkit.Rect{W: testW, H: testH})
	v.root.Draw(painter.NewPixelPainter(buf, testW, testH), toolkit.DefaultDark())
	return buf
}

func click(v *view, w toolkit.Widget) {
	b := w.Bounds()
	ev := toolkit.Event{Kind: toolkit.EventClick, X: b.X + b.W/2, Y: b.Y + b.H/2}
	v.root.OnEvent(ev)
	ev.Kind = toolkit.EventMouseUp
	v.root.OnEvent(ev)
}

func flat(buf []byte) bool {
	for i := 4; i < len(buf); i += 4 {
		if buf[i] != buf[0] || buf[i+1] != buf[1] || buf[i+2] != buf[2] {
			return false
		}
	}
	return true
}

// The view shows what the ViewModel holds, and a click goes back to it.
func TestViewBindsAndDispatches(t *testing.T) {
	f := newFixture(t)
	v := newView(f.vm, f.q, nil)
	buf := layout(t, v)
	if strings.Join(v.servers.Items, "|") != "Demo server" || strings.Join(v.entries.Items, "|") != "files/" {
		t.Fatalf("servers %q, entries %q", v.servers.Items, v.entries.Items)
	}
	if v.user.Text().Get() != "demo" || v.dir.Text().Get() != "/" {
		t.Fatalf("user %q, dir %q", v.user.Text().Get(), v.dir.Text().Get())
	}
	// Among the shares nothing is made, deleted or uploaded: greyed.
	if !v.newFolder.Disabled().Get() || !v.del.Disabled().Get() || !v.upload.Disabled().Get() || v.open.Disabled().Get() {
		t.Fatal("buttons do not follow their commands")
	}
	if flat(buf) {
		t.Fatal("the frame is one colour")
	}
	// A click on Open goes into the share; the next frame shows it.
	click(v, v.open)
	f.settle()
	layout(t, v)
	if v.dir.Text().Get() != "/files/" || !strings.HasPrefix(v.entries.Items[0], "photos/") {
		t.Fatalf("dir %q, entries %q", v.dir.Text().Get(), v.entries.Items)
	}
	if v.newFolder.Disabled().Get() || v.up.Disabled().Get() {
		t.Fatal("in a share, New folder and Up are offered")
	}
}
