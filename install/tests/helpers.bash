# shellcheck shell=bash disable=SC2034 # the settings it sets are read by the sourced install.sh
# Shared setup for the installer's bats tests. Proxmox's tools, curl, and ip are replaced by stubs in a directory at
# the front of PATH; every stub call is appended to $CALLS.

bats_require_minimum_version 1.5.0 # for `run !`

INSTALL_SH="$BATS_TEST_DIRNAME/../install.sh"

setup() {
	STUBS="$BATS_TEST_TMPDIR/bin"
	CALLS="$BATS_TEST_TMPDIR/calls"
	mkdir -p "$STUBS"
	: >"$CALLS"
	export PATH="$STUBS:$PATH" CALLS
	PAR_INSTALL_SOURCED=1
	# shellcheck source=/dev/null # checked on its own
	source "$INSTALL_SH"
	# bats needs -e to fail a test on its first failing line, but its own code reads unset variables.
	set +u
}

# stub NAME BODY: replaces the command NAME with a script that logs its arguments and then runs BODY, in which "$*"
# is the arguments.
stub() {
	cat >"$STUBS/$1" <<EOF
#!/usr/bin/env bash
echo "$1 \$*" >>"\$CALLS"
$2
EOF
	chmod +x "$STUBS/$1"
}

# settings_ok sets valid settings for an organization.
settings_ok() {
	PAR_GITHUB_URL=https://github.com/my-org
	apply_defaults
}
