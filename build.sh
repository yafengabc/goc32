#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")"
EXE="$(go env GOEXE)"
mkdir -p bin
go build -trimpath -ldflags="-s -w" -o "bin/goc32$EXE" .
echo "done: bin/goc32$EXE"
