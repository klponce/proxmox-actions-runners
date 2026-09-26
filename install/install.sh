#!/usr/bin/env bash
# install.sh installs proxmox-actions-runners on a standalone Proxmox VE 9 node: ephemeral GitHub Actions runners in
# VMs, one job per VM. Run it as root on the node. See docs/install.md.
#
#   bash install.sh [--dry-run] [--yes] [parcon install flags...]
#
# It is only a bootstrap: it downloads this release's parcon, checks it against the checksum written in below, and
# runs `parcon install` with its arguments. parcon checks the node, shows its plan, and changes nothing until you
# confirm it; the first thing it then does is install itself as /usr/local/bin/parcon, which manages the install from
# there on (parcon status, parcon config, parcon update, parcon uninstall).
set -euo pipefail

# The release workflow replaces the two placeholders (install/fill-release.sh): the version, such as 0.2.0, and the
# SHA-256 of that release's parcon. A copy from the source tree refuses to run.
PAR_VERSION="@PAR_VERSION@"
PAR_PARCON_SHA256="@PAR_PARCON_SHA256@"

readonly REPO_URL="https://github.com/klponce/proxmox-actions-runners"

die() {
	printf 'error: %s\n' "$*" >&2
	exit 1
}

parcon_url() { echo "$REPO_URL/releases/download/v$PAR_VERSION/parcon-$PAR_VERSION-linux-amd64"; }

main() {
	[[ $PAR_VERSION != @*@ ]] ||
		die "this install.sh is from the source tree; download install.sh from a release: $REPO_URL/releases"
	[[ $(id -u) == 0 ]] || die "run it as root"
	[[ $(dpkg --print-architecture 2>/dev/null) == amd64 ]] || die "proxmox-actions-runners runs on amd64 only"
	local dir
	dir=$(mktemp -d /var/tmp/par-bootstrap.XXXXXX)
	# shellcheck disable=SC2064 # dir is expanded now, on purpose.
	trap "rm -rf '$dir'" EXIT
	echo "Downloading parcon $PAR_VERSION"
	curl -fsSL --retry 3 -o "$dir/parcon" "$(parcon_url)" || die "downloading parcon failed"
	echo "$PAR_PARCON_SHA256  $dir/parcon" | sha256sum -c --quiet - ||
		die "the downloaded parcon doesn't match this install.sh's checksum"
	chmod 0755 "$dir/parcon"
	# Not exec: the trap removes the download once parcon has installed itself.
	"$dir/parcon" install "$@"
}

# Tests source this file with PAR_INSTALL_SOURCED set. Otherwise main runs only once the whole script has arrived, so
# a download cut short can't run half of it.
[[ -n ${PAR_INSTALL_SOURCED:-} ]] || main "$@"
