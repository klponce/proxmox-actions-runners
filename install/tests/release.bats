#!/usr/bin/env bats
# fill-release.sh, which writes a release's version and its parcon's checksum into install.sh.

load helpers

@test "the filled bootstrap carries the version and parcon's checksum" {
	printf '%s  par-runner-0.1.0.qcow2\n%s  parcon-0.1.0-linux-amd64\n' "$(printf a | sha256sum | cut -d' ' -f1)" \
		"$(printf b | sha256sum | cut -d' ' -f1)" >"$BATS_TEST_TMPDIR/sums"
	"$FILL" 0.1.0 "$BATS_TEST_TMPDIR/sums" >"$BATS_TEST_TMPDIR/install.sh"
	bash -n "$BATS_TEST_TMPDIR/install.sh"
	run bash -c 'PAR_INSTALL_SOURCED=1 && source "$1" && echo "$PAR_VERSION" && echo "$PAR_PARCON_SHA256" &&
		parcon_url' _ "$BATS_TEST_TMPDIR/install.sh"
	[ "$status" -eq 0 ]
	[ "${lines[0]}" = 0.1.0 ]
	[ "${lines[1]}" = "$(printf b | sha256sum | cut -d' ' -f1)" ]
	[ "${lines[2]}" = \
		https://github.com/klponce/proxmox-actions-runners/releases/download/v0.1.0/parcon-0.1.0-linux-amd64 ]
}

@test "a pre-release version is accepted" {
	echo parcon >"$BATS_TEST_TMPDIR/parcon"
	filled 0.2.0-rc.1 "$BATS_TEST_TMPDIR/parcon"
	grep -qx 'PAR_VERSION="0.2.0-rc.1"' "$BATS_TEST_TMPDIR/install.sh"
}

@test "bad versions and checksum files are refused" {
	printf '%s  parcon-0.1.0-linux-amd64\n' "$(printf a | sha256sum | cut -d' ' -f1)" >"$BATS_TEST_TMPDIR/sums"
	run "$FILL" v0.1.0 "$BATS_TEST_TMPDIR/sums"
	[ "$status" -eq 1 ]
	[[ $output == *"isn't X.Y.Z"* ]]
	run "$FILL" 0.1.0-RC1 "$BATS_TEST_TMPDIR/sums"
	[ "$status" -eq 1 ]

	echo 'abc  "$(reboot)"' >"$BATS_TEST_TMPDIR/bad"
	run "$FILL" 0.1.0 "$BATS_TEST_TMPDIR/bad"
	[ "$status" -eq 1 ]
	[[ $output == *"not a sha256sum line"* ]]

	# The sums must have this release's parcon.
	run "$FILL" 0.2.0 "$BATS_TEST_TMPDIR/sums"
	[ "$status" -eq 1 ]
	[[ $output == *"has no parcon-0.2.0-linux-amd64"* ]]

	: >"$BATS_TEST_TMPDIR/empty"
	run "$FILL" 0.1.0 "$BATS_TEST_TMPDIR/empty"
	[ "$status" -eq 1 ]
}
