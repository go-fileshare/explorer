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
  name. Nobody sees half a file under its name. The portal takes 16 MiB per PUT.
- **An upload that fails can be resumed** (since v0.3.0). The part file is named
  after the file's fingerprint (name, size, modification time):
  `<name>.<fingerprint>.part`. When the same file is uploaded again, the explorer
  asks the part's size (`PROPFIND`, Depth 0), reads its last 64 KiB back (a
  `Range` GET), and continues from there only if those bytes are the file's.
  Otherwise it starts again, so a different file with the same name is never
  continued. A failure of the network or the server keeps the part, and the
  explorer says so. A refusal (403, a full share: 507) or a cancellation removes
  it. Part files are hidden from the listing.
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

## Embedded by a page that holds the files

A page that has the files itself can embed the explorer without a portal (since
v0.4.0). The [go-filesystems playground](https://go-filesystems.github.io/playground/)
does this: its engine serves an opened disk image's WebDAV inside the tab. The
page puts the explorer's files beside its own, opens `explorer.html` in a
**same-origin iframe**, and gives it a bridge:

```js
window.fileshareExplorerBridge = {
  label: "disk.img",                        // the one server's name
  request(method, path, headers, body) {    // -> Promise<{status, headers, body}>
    /* answer the WebDAV request; headers are [name, value] pairs, body a Uint8Array */
  },
  download(path, name) { /* save the file */ },
  close() { /* "Log out": leave the explorer */ },
  state(json) { /* optional: {status, dir, entries}, what the explorer shows */ },
};
```

The explorer finds the bridge on its parent and uses it instead of the portal.
There is no login and no token, and no request leaves the page. A parent of
another origin cannot be read, so it is never taken for a bridge. Each body is
bounded either way (16 MiB), and uploads go in 4 MiB pieces.

## Natively

```sh
go install github.com/go-fileshare/explorer@latest
explorer                    # reads ~/Library/Application Support/fileshare-explorer/explorer.hcl (macOS)
explorer -config my.hcl     # or $XDG_CONFIG_HOME/fileshare-explorer/explorer.hcl elsewhere
explorer -demo              # over a built-in stand-in: real WebDAV over a temporary directory
```

Natively there is no portal: the explorer logs in itself and calls each
server's WebDAV with a token addressed to that server.

```hcl
# explorer.hcl
issuer "https://login.example.org" {
  client_id = "fileshare-explorer"     # a PUBLIC client: no secret
}

server "paris" {
  label    = "Paris"
  dav_url  = "https://fs-paris.example.org"   # fileshare's WebDAV, over TLS
  resource = "https://fs-paris.example.org/"  # the audience of its oidc block
  # ca_file = "paris-ca.pem"                  # when its certificate is not public
}
```

- **Login** goes through the system browser and a loopback listener, as a public
  client ([go-authn/rp](https://github.com/go-authn/rp), RFC 8252). The refresh
  token stays in the program's memory and is never written anywhere. It is spent
  one refresh at a time, because a provider that rotates refresh tokens ends the
  login when one is spent twice.
- **Each server gets its own token**, for its `resource` (RFC 8707). A server
  that refuses a token gets a new one, once. A `dav_url` that is not `https` is
  refused: a bearer token over cleartext is anybody's.
- **Uploading** opens the explorer's own picker in place of the server's files:
  a folder of this computer to move through, with hidden items left out. A file
  is read one piece at a time, reopened for each piece, so nothing stays open
  once the upload is over. A file that changes while it is being sent fails the
  upload rather than sending a mixture. (go-widgets has no native file dialog
  yet.)
- **Downloads** stream into `~/Downloads`. A download never replaces a file
  already there, and a name the server gives is refused when it is not a plain
  file name on this system (`..\x`, `c:x`, and so on).

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
- **Where the servers are** is a `backend`: the portal in a browser, or
  `internal/direct` natively, which is never linked into the page.

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
- **the native backend against the same real fileshare**: it logs in through a
  loopback listener, lists the shares, uploads and downloads 20 MiB, and has a
  token for another audience refused (the control).

```sh
cd e2e
PORTAL=$(go env GOPATH)/bin/portal FILESHARE=$(go env GOPATH)/bin/fileshare go test -v ./...
EXPLORER_SCREENSHOT=/some/dir/outside/any/repo/explorer.png ...   # to look at what the test saw
```

The screenshot is refused inside a git work tree: a capture one `git add -A` away
from a public repository is how pictures get published by accident.

## Not yet

- The system's own file dialog natively (the explorer's picker chooses one file
  at a time).
- Removing part files that nobody comes back for: a failed upload that is never
  retried leaves its part on the server, hidden from the listing.
- Drag and drop, and several selected entries at once.

## Licence

BSD 3-Clause.
