#!/usr/bin/env bash
# Verify the public Go install path from outside the repository.
set -euo pipefail

MODULE="github.com/jonbaldie/gastown"
REF="${1:-main}"
TMPDIR="$(mktemp -d)"
trap 'rm -rf "$TMPDIR"' EXIT

mkdir -p "$TMPDIR/bin" "$TMPDIR/work"
cd "$TMPDIR/work"

GOWORK=off GOPROXY=direct CGO_ENABLED=0 GOBIN="$TMPDIR/bin" \
  go install "$MODULE/cmd/gt@$REF"

go version -m "$TMPDIR/bin/gt" | grep -F "path\t$MODULE/cmd/gt"
go version -m "$TMPDIR/bin/gt" | grep -F "mod\t$MODULE "
"$TMPDIR/bin/gt" version
