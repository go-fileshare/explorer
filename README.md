# explorer

The file explorer for [go-fileshare](https://github.com/go-fileshare/fileshare):
browse, upload, download, rename, copy and delete the files of every share you may
use, on **every fileshare server** a [portal](https://github.com/go-fileshare/portal)
knows. It is written once in Go with [go-widgets](https://github.com/go-widgets)
and runs **in a browser (wasm)** or **as a native window**. Pure Go,
`CGO_ENABLED=0`.

This is phase 4 of the [UI design](https://github.com/go-fileshare/fileshare/blob/main/docs/ui.md);
the admin console, phase 3, is [go-fileshare/console](https://github.com/go-fileshare/console).

## In a browser, behind the portal

```sh
./build.sh                                  # dist/: explorer.wasm(.gz), wasm_exec.js, the page
sudo cp -r dist /usr/share/fileshare-explorer
```

```hcl
# portal.hcl (go-fileshare/portal v0.2.0 or later)
server "paris" {
  url          = "https://fs-paris.example.org:8443"   # the admin API
  issuer       = "https://id.example.org"
  resource     = "https://fs-paris.example.org/"
  dav_url      = "https://fs-paris.example.org"        # its WebDAV: what the explorer browses
  dav_resource = "https://fs-paris.example.org/dav"    # only if fileshare's oidc audience differs
}
ui "explorer" { dir = "/usr/share/fileshare-explorer" }
```

Open `https://<portal>/explorer/`. Without a session the explorer sends you to log
in at your identity provider and comes back. It lists the servers whose WebDAV the
portal passes, opens the first on the shares you may use, and every request goes to
the portal as `/dav/<server>/<share>/<path>`. The portal adds that server's token;
the page never holds one. It uses
[`portal/client`](https://github.com/go-fileshare/portal#the-uis-side-client), as
the console does: one refresh at a time across tabs (Web Locks), and the portal's
CSRF header on every call.

The servers must be **fileshare v0.29.0 or later**. That version appends an upload
piece at the end of a file, and answers a PROPFIND of its root with the shares.
Earlier versions answer the root with an HTML page, and refuse the first piece of
an upload with 416.

### What it does, and how

- **Uploads go in pieces, never whole.** A browser cannot stream a request body
  (except Chrome over HTTP/2), and Go's wasm `net/http` reads a body whole before
  sending it. So a file is read 8 MiB at a time from the browser's `File`
  (`slice().arrayBuffer()`): first an empty PUT of `<name>.part`, then each piece
  with a `Content-Range` that starts where the last one ended, then a MOVE to the
  name. Nobody sees half a file under its name, and a failure removes the part
  file. The portal takes 16 MiB per PUT.
- **Nothing is replaced without asking.** A rename, a copy or an upload onto a name
  that is taken is refused (`Overwrite: F`; RFC 4918 makes `T` the default, which
  would silently destroy the file a rename lands on). Deleting takes two presses,
  and the second press counts only if the selection has not changed in between.
- **Downloads are a navigation.** The explorer first makes sure the portal holds a
  current token for the server, because a navigation that meets "refresh first"
  cannot refresh and try again. Then the browser follows a link. The portal serves
  the file as an inert attachment, so a page on a share never runs as the portal.
- **Names are names.** `a/b`, `.` and `..` are refused before anything is sent.

The page weighs **4.6 MB compressed** (18.5 MB of wasm, as `build.sh` builds it;
the portal sends the `.gz`).

## Natively

```sh
go install github.com/go-fileshare/explorer@latest
explorer -demo              # over a built-in stand-in: real WebDAV over a temporary directory
```

The native explorer does not log in on its own yet. `-demo` opens it over a
stand-in portal with one server and a share of sample files. Downloads stream into
`~/Downloads`; one never replaces a file already there, and a name the server
gives is refused when it is not a plain file name on this system (`..\x`, `c:x`,
and so on). Uploading needs a native file picker, which is not there yet.

## How it is built

MVVM, as in every go-widgets application:

- **The ViewModel** (`viewmodel.go`) holds `mvvm` observables and commands and
  knows no widget. Work runs on its own goroutine; results come back through an
  `mvvm.Queue` and are applied on the UI goroutine. A result about a folder no
  longer shown is dropped.
- **The View** (`scene.go`) is toolkit widgets bound with `mvvmtk`, and writes no
  widget state of its own.
- **WebDAV** (`internal/dav`) is the part of RFC 4918 that browsing needs, and
  nothing more: no locks, and no properties beyond the four a listing shows.

CI runs:

- vet and tests on Linux, macOS and Windows, plus the browser build;
- the race detector and a recorded coverage floor;
- the `mvvmlint` and `bricolint` gates (bricolint with a negative control that
  shows it still fires);
- govulncheck;
- **the explorer as deployed**: `explorer.wasm` in headless Chrome, behind the
  portal binary over TLS, in front of a fileshare binary's WebDAV, with a test
  OpenID provider. It logs in, lists the shares, uploads 20 MiB in three pieces,
  makes a folder, is refused a rename onto an existing file, downloads one and
  deletes the folder. Each step is checked on fileshare's disk.

```sh
cd e2e
PORTAL=$(go env GOPATH)/bin/portal FILESHARE=$(go env GOPATH)/bin/fileshare go test -v ./...
EXPLORER_SCREENSHOT=/some/dir/outside/any/repo/explorer.png ...   # to look at what the test saw
```

The screenshot is refused inside a git work tree: a capture one `git add -A` away
from a public repository is how pictures get published by accident.

## Not yet

- Native login (through `go-authn/rp`, as the console does) and a native file
  picker.
- Resuming an upload after a failure: the pieces would allow it, but today the
  part file is removed.
- Drag and drop, and several selected entries at once.

## Licence

BSD 3-Clause.
