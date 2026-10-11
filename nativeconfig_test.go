// SPDX-License-Identifier: BSD-3-Clause

//go:build !(js && wasm)

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const nativeBase = `
issuer "https://login.example.org" {
  client_id = "fileshare-explorer"
}
server "paris" {
  label    = "Paris"
  dav_url  = "https://fs-paris.example.org"
  resource = "https://fs-paris.example.org/"
  ca_file  = "ca.pem"
}
`

func writeConfig(t *testing.T, text string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "explorer.hcl")
	if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestNativeConfig(t *testing.T) {
	p := writeConfig(t, nativeBase)
	c, err := loadNative(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.Issuer != "https://login.example.org" || c.ClientID != "fileshare-explorer" || len(c.Servers) != 1 {
		t.Fatalf("%+v", c)
	}
	s := c.Servers[0]
	if s.ID != "paris" || s.Label != "Paris" || s.DAVURL != "https://fs-paris.example.org" ||
		s.Resource != "https://fs-paris.example.org/" || s.CAFile != filepath.Join(filepath.Dir(p), "ca.pem") {
		t.Fatalf("%+v", s)
	}
	// An absolute CA path is kept as it is.
	abs := filepath.Join(t.TempDir(), "ca.pem")
	c, err = loadNative(writeConfig(t, strings.Replace(nativeBase, `"ca.pem"`, `"`+filepath.ToSlash(abs)+`"`, 1)))
	if err != nil || filepath.Clean(c.Servers[0].CAFile) != abs {
		t.Fatalf("%v %q", err, c.Servers[0].CAFile)
	}
	// Each refusal changes one thing in a configuration that is otherwise
	// accepted (the base, above).
	for name, change := range map[string][2]string{
		"not HCL":           {nativeBase, nativeBase + "}"},
		"unknown field":     {nativeBase, nativeBase + "x = 1\n"},
		"issuer in clear":   {`"https://login.example.org"`, `"http://login.example.org"`},
		"WebDAV in clear":   {`"https://fs-paris.example.org"`, `"http://fs-paris.example.org"`},
		"no client_id":      {`client_id = "fileshare-explorer"`, ``},
		"no dav_url":        {`dav_url  = "https://fs-paris.example.org"`, ``},
		"the old url field": {`dav_url `, `url     `},
	} {
		text := strings.Replace(nativeBase, change[0], change[1], 1)
		if text == nativeBase {
			t.Fatalf("%s: the case changes nothing", name)
		}
		if _, err := loadNative(writeConfig(t, text)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// A provider on this computer may be plain HTTP (a test, a local one).
	if _, err := loadNative(writeConfig(t, strings.Replace(nativeBase, `"https://login.example.org"`, `"http://127.0.0.1:9000"`, 1))); err != nil {
		t.Errorf("a loopback provider: %v", err)
	}
	if _, err := loadNative(filepath.Join(t.TempDir(), "none.hcl")); err == nil {
		t.Error("a missing file loaded")
	}
	if !strings.HasSuffix(defaultConfigPath(), filepath.Join("fileshare-explorer", "explorer.hcl")) {
		t.Error(defaultConfigPath())
	}
}
