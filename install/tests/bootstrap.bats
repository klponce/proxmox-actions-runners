#!/usr/bin/env bats
# install.sh, the bootstrap: it downloads the release's parcon, checks it, and runs `parcon install`.

load helpers

# node stubs a root amd64 node whose download serves a parcon that prints CONTENT and records how it was run.
node() {
	stub id 'echo 0'
	stub dpkg 'echo amd64'
	printf '#!/usr/bin/env bash\necho "parcon $*" >>"$CALLS"\necho %q\n' "$1" >"$BATS_TEST_TMPDIR/parcon"
	stub curl 'out=""; while (($#)); do [[ $1 == -o ]] && out=$2; shift; done; cp "'"$BATS_TEST_TMPDIR"'/parcon" "$out"'
}

@test "it runs the verified parcon install with the script's arguments" {
	node "the real parcon"
	filled 0.2.0 "$BATS_TEST_TMPDIR/parcon"
	run bash "$BATS_TEST_TMPDIR/install.sh" --dry-run --bridge vmbr1
	[ "$status" -eq 0 ]
	[[ $output == *"the real parcon"* ]]
	grep -q '^curl .*releases/download/v0.2.0/parcon-0.2.0-linux-amd64$' "$CALLS"
	grep -qx 'parcon install --dry-run --bridge vmbr1' "$CALLS"
}

@test "a parcon that doesn't match the checksum never runs" {
	node "the real parcon"
	echo "some other parcon" >"$BATS_TEST_TMPDIR/other"
	filled 0.2.0 "$BATS_TEST_TMPDIR/other"
	run bash "$BATS_TEST_TMPDIR/install.sh"
	[ "$status" -eq 1 ]
	[[ $output == *"doesn't match this install.sh's checksum"* ]]
	run ! grep -q '^parcon ' "$CALLS"
}

@test "it refuses to run from the source tree, as a user, or off amd64" {
	node "the real parcon"
	run bash "$INSTALL_SH"
	[ "$status" -eq 1 ]
	[[ $output == *"from the source tree"* ]]

	filled 0.2.0 "$BATS_TEST_TMPDIR/parcon"
	stub id 'echo 1000'
	run bash "$BATS_TEST_TMPDIR/install.sh"
	[ "$status" -eq 1 ]
	[[ $output == *"run it as root"* ]]

	stub id 'echo 0'
	stub dpkg 'echo arm64'
	run bash "$BATS_TEST_TMPDIR/install.sh"
	[ "$status" -eq 1 ]
	[[ $output == *"amd64 only"* ]]
	run ! grep -q '^curl ' "$CALLS"
}
