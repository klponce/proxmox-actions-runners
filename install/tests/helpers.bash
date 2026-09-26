# shellcheck shell=bash disable=SC2034 # INSTALL_SH and FILL are read by the tests that load this
# Shared setup for the bootstrap's bats tests. Commands are replaced by stubs in a directory at the front of PATH;
# every stub call is appended to $CALLS.

bats_require_minimum_version 1.5.0 # for `run !`

INSTALL_SH="$BATS_TEST_DIRNAME/../install.sh"
FILL="$BATS_TEST_DIRNAME/../fill-release.sh"

setup() {
	STUBS="$BATS_TEST_TMPDIR/bin"
	CALLS="$BATS_TEST_TMPDIR/calls"
	mkdir -p "$STUBS"
	: >"$CALLS"
	export PATH="$STUBS:$PATH" CALLS
}

# stub NAME BODY: replaces the command NAME with a script that logs its arguments and then runs BODY, in which "$*"
# is the arguments.
stub() {
	# shellcheck disable=SC2016 # the stub expands $* and $CALLS when it runs.
	printf '#!/usr/bin/env bash\necho "%s $*" >>"$CALLS"\n%s\n' "$1" "$2" >"$STUBS/$1"
	chmod +x "$STUBS/$1"
}

# filled VERSION FILE: install.sh filled for a release whose parcon is FILE, in $BATS_TEST_TMPDIR/install.sh.
filled() {
	printf '%s  parcon-%s-linux-amd64\n' "$(sha256sum <"$2" | cut -d' ' -f1)" "$1" >"$BATS_TEST_TMPDIR/sums"
	"$FILL" "$1" "$BATS_TEST_TMPDIR/sums" >"$BATS_TEST_TMPDIR/install.sh"
}
