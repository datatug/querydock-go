#!/usr/bin/env bash
set -euo pipefail
export GOWORK=off
go test ./... -coverpkg=./... -coverprofile=coverage.out
go run ./cmd/coveragegate coverage.out
go tool cover -func=coverage.out
total=$(go tool cover -func=coverage.out | awk '$1 == "total:" { print $3 }')
[ "$total" = '100.0%' ] || { echo "Expected exactly 100.0%, got $total" >&2; exit 1; }
