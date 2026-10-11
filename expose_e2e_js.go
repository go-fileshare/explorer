// SPDX-License-Identifier: BSD-3-Clause

//go:build js && wasm && e2e

package main

import (
	"encoding/json"
	"syscall/js"
)

// expose lets the browser test read the ViewModel and drive it, built with
// -tags e2e only, never in the explorer a portal serves.
func expose(vm *viewModel) {
	js.Global().Set("explorerState", js.FuncOf(func(js.Value, []js.Value) any {
		var entries []string
		for _, e := range vm.Entries.Slice() {
			entries = append(entries, e.Name)
		}
		b, _ := json.Marshal(map[string]any{"user": vm.User.Get(), "status": vm.Status.Get(), "dir": vm.Dir.Get(),
			"entries": entries, "busy": vm.Busy.Get()})
		return string(b)
	}))
	// explorerDo runs a command by name, as its button would.
	js.Global().Set("explorerDo", js.FuncOf(func(_ js.Value, a []js.Value) any {
		cmds := map[string]func(){"open": vm.Open.Execute, "up": vm.Up.Execute, "download": vm.Download.Execute,
			"upload": vm.Upload.Execute, "delete": vm.Delete.Execute, "newFolder": vm.NewFolder.Execute,
			"rename": vm.Rename.Execute, "duplicate": vm.Duplicate.Execute}
		// The selection first: selecting an entry puts its name in the field.
		if len(a) > 2 {
			vm.EntrySel.Set(a[2].Int())
		}
		if len(a) > 1 && a[1].String() != "" {
			vm.Name.Set(a[1].String())
		}
		cmds[a[0].String()]()
		return nil
	}))
}
