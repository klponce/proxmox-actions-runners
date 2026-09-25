#!/usr/bin/env bash
# build-image.sh IMAGE VERSION builds one release image (runner, gateway, or controller) with Packer and boot-tests
# it. It runs in the dev container with KVM, in the release workflow or locally. The image and its checksum and
# manifest land in output-IMAGE/.
set -euo pipefail
cd "$(dirname "$0")/../.."

main() {
	local image=${1:?usage: build-image.sh IMAGE VERSION} version=${2:?usage: build-image.sh IMAGE VERSION}
	local args=(-var "version=$version") name expect=() nics=1
	case $image in
	runner)
		name=par-runner-$version
		expect=('Started.*par-runner.path')
		;;
	gateway)
		name=par-gateway-$version
		expect=('Finished.*nftables.service' 'Started.*dnsmasq.service')
		nics=2
		;;
	controller)
		name=parcon-$version
		mkdir -p bin
		CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$version" -o bin/parcon ./cmd/parcon
		args+=(-var parcon_binary=bin/parcon)
		;;
	*)
		echo "unknown image $image; want runner, gateway, or controller" >&2
		exit 2
		;;
	esac

	rm -rf "output-$image"
	packer init "images/$image"
	packer build -color=false "${args[@]}" "images/$image"
	NICS=$nics images/common/boot-test.sh "output-$image/$name.qcow2" "${expect[@]}"
	ls -l "output-$image"
}

main "$@"
