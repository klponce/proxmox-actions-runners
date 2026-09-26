#!/usr/bin/env bash
# check.sh runs every check a change must pass: the Go build, tests, vet, format, and lint; shellcheck; the
# installer's bats tests; Packer's format and validation; and actionlint on the workflows. It runs in the dev
# container, locally and as the first job of the release workflow (.github/workflows/release.yml), which builds the
# images next. It builds nothing that takes long, and the integration suite needs a Proxmox node.
set -euo pipefail
cd "$(dirname "$0")/../.."

step() { printf '\n== %s\n' "$*"; }

step "go build"
go build ./...
step "go test"
go test -race ./...
step "go vet"
go vet ./...
go vet -tags integration ./...
step "gofmt"
unformatted=$(gofmt -l .)
[[ -z $unformatted ]] || { echo "not formatted: $unformatted" && exit 1; }
step "golangci-lint"
golangci-lint run

step "shellcheck"
shellcheck images/common/*.sh images/*/scripts/*.sh images/*/tests/*.sh images/gateway/par-gateway-configure \
	test/integration/*.sh test/integration/node/*.sh install/*.sh install/tests/helpers.bash .github/scripts/*.sh
step "bats"
bats install/tests .github/scripts/tests

step "packer"
packer fmt -check -recursive images
bin=$(mktemp -d)
trap 'rm -rf "$bin"' EXIT
CGO_ENABLED=0 go build -o "$bin/parcon" ./cmd/parcon
for image in runner gateway controller; do
	packer init "images/$image" >/dev/null
	# validate refuses to run when a build's output directory exists, so point it somewhere empty.
	args=(-var output_directory="$bin/output-$image")
	[[ $image != controller ]] || args+=(-var parcon_binary="$bin/parcon")
	packer validate "${args[@]}" "images/$image"
done

step "actionlint"
actionlint

echo
echo "all checks passed"
