// SPDX-License-Identifier: BSD-3-Clause

//go:build js && wasm && !e2e

package main

// expose does nothing outside the browser test (expose_e2e_js.go).
func expose(*viewModel) {}
