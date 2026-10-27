#!/bin/sh
# Read-only source-delivery parity checker. Run against a quiescent mirror or
# snapshot. --original-decoder recreates v0.17.57's decoder and absence of the
# decoded-object integrity guard in a Go build overlay, never in the checkout.
set -eu
cd "$(dirname "$0")/.."
if [ "${1:-}" != --original-decoder ]; then
  exec go run ./hack/archive-parity "$@"
fi
shift
scratch=$(mktemp -d)
trap 'rm -rf "$scratch"' EXIT HUP INT TERM
python3 - "$PWD" "$scratch" <<'PY'
import json
import pathlib
import sys
root, scratch = map(pathlib.Path, sys.argv[1:])
replacements = {}
for relative, old, new in [
    ('third_party/go-git/plumbing/format/packfile/patch_delta.go',
     '\t\t\t\t\tbasePos = 0\n', ''),
    ('internal/gitcache/backend_verified.go',
     'return verifiedObject{EncodedObject: object, expected: hash}, nil',
     'return object, nil'),
]:
    source = root / relative
    body = source.read_text()
    if body.count(old) != 1:
        raise SystemExit('original-decoder control no longer matches ' + relative)
    target = scratch / source.name
    target.write_text(body.replace(old, new))
    replacements[str(source)] = str(target)
(scratch / 'overlay.json').write_text(json.dumps({'Replace': replacements}))
PY
go run -overlay "$scratch/overlay.json" ./hack/archive-parity "$@"
