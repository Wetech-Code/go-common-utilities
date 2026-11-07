#!/usr/bin/env bash
# Runs every check that must pass before a pull request is merged or a tag is cut.
# Needs only Go. Integration tests are separate (see README).
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."

echo "== gofmt";        test -z "$(gofmt -l .)" || { gofmt -l .; exit 1; }
echo "== go mod tidy";  go mod tidy; git diff --exit-code go.mod go.sum
echo "== go mod verify"; go mod verify
echo "== go vet";       go vet ./... && go vet -tags integration ./...
echo "== go test -race"; go test -race -count=1 ./...
echo "== consumer";     scripts/consumer-check.sh
echo "== govulncheck";  go run golang.org/x/vuln/cmd/govulncheck@v1.9.0 ./...
echo "all checks passed"
