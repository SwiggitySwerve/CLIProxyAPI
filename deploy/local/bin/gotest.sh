#!/usr/bin/env bash
# gotest.sh [packages...]: gofmt -l (fails if any file is listed), go vet and go test
# for the given packages (default ./...), run in the Go container with cache volumes.
# Go is not installed on the host.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../../.."

[ $# -gt 0 ] || set -- ./...

exec podman run --rm -v "$PWD":/src:Z -w /src \
  -v go-mod-cache:/go/pkg/mod -v go-build-cache:/root/.cache/go-build \
  docker.io/library/golang:1.26-bookworm bash -euo pipefail -c '
    unformatted=$(go list -f "{{.Dir}}" "$@" | sort -u | while read -r d; do gofmt -l "$d"/*.go; done)
    if [ -n "$unformatted" ]; then
      echo "gofmt: these files need formatting:" >&2
      echo "$unformatted" >&2
      exit 1
    fi
    echo "gofmt: clean"
    go vet "$@"
    echo "go vet: clean"
    go test "$@"
  ' _ "$@"
