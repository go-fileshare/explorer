#!/usr/bin/env bash
# Negative control for the bricolint hand-drawn-UI guard: a green run only
# proves the guard is silent on this tree, not that it can still bite. So:
#
#   1. clean tree                                   -> exit 0
#   2. add a file that draws with painter directly  -> exit != 0
#   3. remove it                                    -> exit 0
#
# The explorer draws only through toolkit widgets, so there is no painter call
# of its own to splice into; the probe is a file of the same package calling a
# real drawing primitive on a real *painter.PixelPainter.
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
bricolint="${BRICOLINT:-$(go env GOPATH)/bin/bricolint}"
probe="$root/zz_bricolint_probe.go"
[ -x "$bricolint" ] || { echo "error: no bricolint at $bricolint" >&2; exit 2; }
[ ! -e "$probe" ] || { echo "error: $probe exists already" >&2; exit 2; }
trap 'rm -f "$probe"' EXIT
guard() { ( cd "$root" && GOWORK=off go vet -vettool="$bricolint" ./... ) >/dev/null 2>&1; echo $?; }

echo "==> 1/3 clean tree"
[ "$(guard)" = 0 ] || { echo "FAIL: not green on the clean tree" >&2; exit 1; }
echo "==> 2/3 a raw painter draw call"
cat > "$probe" <<'GO'
package main

import "github.com/go-widgets/painter"

func bricolintProbe(buf []byte) {
	p := painter.NewPixelPainter(buf, 1, 1)
	p.FillRect(painter.Rect{}, painter.RGBA{})
}
GO
[ "$(guard)" != 0 ] || { echo "FAIL: the guard stayed green on a raw FillRect: it does not bite" >&2; exit 1; }
echo "==> 3/3 removed"
rm -f "$probe"
[ "$(guard)" = 0 ] || { echo "FAIL: not green again after removing the probe" >&2; exit 1; }
echo "PASS: bricolint bites on a raw painter call and is silent otherwise"
