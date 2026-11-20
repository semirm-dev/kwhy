#!/usr/bin/env bash
# Pins k8s.io/kubernetes and the staging modules it requires as v0.0.0 (why: docs/plans/backlog.md, "No go install").
# Usage: scripts/k8s-replace.sh v1.37.2   (run from anywhere; edits the repo's go.mod)
set -euo pipefail
v=${1:?usage: $0 v1.MINOR.PATCH}
cd "$(dirname "$0")/.."

gomod=$(go mod download -json "k8s.io/kubernetes@$v" | sed -n 's/.*"GoMod": "\(.*\)".*/\1/p')
for m in $(awk '$1 ~ /^k8s\.io\// && $2 == "v0.0.0" {print $1}' "$gomod"); do
	go mod edit -replace "$m=$m@v0.${v#v1.}"
done
go get "k8s.io/kubernetes@$v"
