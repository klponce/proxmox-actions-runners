#!/usr/bin/env bash
# fill-release.sh VERSION SUMS_FILE prints install/install.sh with the release's version and asset checksums in place
# of its @PAR_VERSION@ and @PAR_SHA256SUMS@ placeholders. The release workflow runs it; the filled script is the
# trust anchor for every asset the installer downloads. SUMS_FILE holds sha256sum's output for the assets.
set -euo pipefail

die() {
	echo "fill-release.sh: $*" >&2
	exit 1
}

main() {
	local version=${1:?usage: fill-release.sh VERSION SUMS_FILE} sums_file=${2:?usage: fill-release.sh VERSION SUMS_FILE}
	# The version ends up in file names, URLs, and Proxmox tags, which allow only lowercase letters, digits, and . -
	[[ $version =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[0-9a-z.]+)?$ ]] || die "version $version isn't X.Y.Z or X.Y.Z-pre"
	local sums
	sums=$(<"$sums_file")
	[[ -n $sums ]] || die "$sums_file is empty"
	local line
	while IFS= read -r line; do
		[[ $line =~ ^[0-9a-f]{64}\ \ [A-Za-z0-9._-]+$ ]] || die "not a sha256sum line: $line"
	done <<<"$sums"

	VERSION=$version SUMS=$sums perl -0pe '
		s/^PAR_VERSION="\@PAR_VERSION\@"$/PAR_VERSION="$ENV{VERSION}"/m or die "no PAR_VERSION placeholder\n";
		s/^PAR_SHA256SUMS="\@PAR_SHA256SUMS\@"$/PAR_SHA256SUMS="$ENV{SUMS}"/m or die "no PAR_SHA256SUMS placeholder\n";
	' "$(dirname "$0")/install.sh"
}

main "$@"
