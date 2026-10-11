// SPDX-License-Identifier: BSD-3-Clause

//go:build !(js && wasm)

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/gohcl"
	"github.com/hashicorp/hcl/v2/hclparse"

	"github.com/go-fileshare/explorer/internal/direct"
)

// The native explorer's configuration: where to log in, and the servers'
// WebDAV -- what a portal would know.
//
//	issuer "https://login.example.org" {
//	  client_id = "fileshare-explorer"     # a PUBLIC client: no secret
//	}
//
//	server "paris" {
//	  label    = "Paris"
//	  dav_url  = "https://fs-paris.example.org"   # fileshare's WebDAV, over TLS
//	  resource = "https://fs-paris.example.org/"  # the audience of its oidc block
//	}
type nativeConfig struct {
	Issuer  issuerConf   `hcl:"issuer,block"`
	Servers []serverConf `hcl:"server,block"`
}

type issuerConf struct {
	URL      string   `hcl:"url,label"`
	ClientID string   `hcl:"client_id"`
	Scopes   []string `hcl:"scopes,optional"`
}

type serverConf struct {
	ID       string `hcl:"id,label"`
	Label    string `hcl:"label,optional"`
	DAVURL   string `hcl:"dav_url"`
	Resource string `hcl:"resource,optional"`
	Scope    string `hcl:"scope,optional"`
	CAFile   string `hcl:"ca_file,optional"`
}

// defaultConfigPath is explorer.hcl in the user's configuration directory:
// ~/Library/Application Support/fileshare-explorer on macOS,
// $XDG_CONFIG_HOME/fileshare-explorer elsewhere.
func defaultConfigPath() string {
	d, err := os.UserConfigDir()
	if err != nil {
		return "explorer.hcl"
	}
	return filepath.Join(d, "fileshare-explorer", "explorer.hcl")
}

// loadNative reads path into the direct backend's configuration.
func loadNative(path string) (direct.Config, error) {
	parser := hclparse.NewParser()
	f, diags := parser.ParseHCLFile(path)
	if diags.HasErrors() {
		return direct.Config{}, diagError(parser, diags)
	}
	var c nativeConfig
	if diags := gohcl.DecodeBody(f.Body, nil, &c); diags.HasErrors() {
		return direct.Config{}, diagError(parser, diags)
	}
	if !strings.HasPrefix(c.Issuer.URL, "https://") && !strings.HasPrefix(c.Issuer.URL, "http://127.0.0.1") {
		return direct.Config{}, fmt.Errorf("%s: issuer %q is not https", path, c.Issuer.URL)
	}
	out := direct.Config{Issuer: c.Issuer.URL, ClientID: c.Issuer.ClientID, Scopes: c.Issuer.Scopes}
	for _, s := range c.Servers {
		if !strings.HasPrefix(s.DAVURL, "https://") {
			// ⛔ A bearer token over cleartext is anybody's.
			return direct.Config{}, fmt.Errorf("%s: server %q: dav_url %q is not https", path, s.ID, s.DAVURL)
		}
		ca := s.CAFile
		if ca != "" && !filepath.IsAbs(ca) {
			ca = filepath.Join(filepath.Dir(path), ca)
		}
		out.Servers = append(out.Servers, direct.Server{ID: s.ID, Label: s.Label, DAVURL: s.DAVURL,
			Resource: s.Resource, Scope: s.Scope, CAFile: ca})
	}
	return out, nil
}

func diagError(parser *hclparse.Parser, diags hcl.Diagnostics) error {
	var b strings.Builder
	wr := hcl.NewDiagnosticTextWriter(&b, parser.Files(), 0, false)
	_ = wr.WriteDiagnostics(diags)
	return errors.New(strings.TrimSpace(b.String()))
}
