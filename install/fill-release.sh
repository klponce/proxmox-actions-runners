#!/usr/bin/env bash
# fill-release.sh VERSION SUMS_FILE prints install/install.sh with the release's version and its parcon's checksum in
# place of the @PAR_VERSION@ and @PAR_PARCON_SHA256@ placeholders. The release workflow runs it; the filled script
# trusts only that one checksum, and parcon verifies everything else against the release's signed SHA256SUMS.
# SUMS_FILE holds sha256sum's output for the assets.
set -euo pipefail

die() {
	echo "fill-release.sh: $*" >&2
	exit 1
}

main() {
	local version=${1:?usage: fill-release.sh VERSION SUMS_FILE} sums_file=${2:?usage: fill-release.sh VERSION SUMS_FILE}
	# The version ends up in file names, URLs, and Proxmox tags, which allow only lowercase letters, digits, and . -
	[[ $version =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[0-9a-z.]+)?$ ]] || die "version $version isn't X.Y.Z or X.Y.Z-pre"
	local line sum=""
	while IFS= read -r line; do
		[[ $line =~ ^[0-9a-f]{64}\ \ [A-Za-z0-9._-]+$ ]] || die "not a sha256sum line: $line"
		[[ ${line#*  } == "parcon-$version-linux-amd64" ]] && sum=${line%%  *}
	done <"$sums_file"
	[[ -n $sum ]] || die "$sums_file has no parcon-$version-linux-amd64"

	VERSION=$version SUM=$sum perl -0pe '
		s/^PAR_VERSION="\@PAR_VERSION\@"$/PAR_VERSION="$ENV{VERSION}"/m or die "no PAR_VERSION placeholder\n";
		s/^PAR_PARCON_SHA256="\@PAR_PARCON_SHA256\@"$/PAR_PARCON_SHA256="$ENV{SUM}"/m or
			die "no PAR_PARCON_SHA256 placeholder\n";
	' "$(dirname "$0")/install.sh"
}

main "$@"
