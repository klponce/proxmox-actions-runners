#!/usr/bin/env bats
# fill-release.sh, which writes a release's version and checksums into install.sh.

load helpers

FILL="$BATS_TEST_DIRNAME/../fill-release.sh"

sums() {
	cat >"$BATS_TEST_TMPDIR/sums" <<EOF
$(printf a | sha256sum | cut -d' ' -f1)  par-runner-12.qcow2
$(printf b | sha256sum | cut -d' ' -f1)  parcon-12-linux-amd64
EOF
}

@test "the filled installer carries the version and every checksum" {
	sums
	"$FILL" 12 "$BATS_TEST_TMPDIR/sums" >"$BATS_TEST_TMPDIR/install.sh"
	bash -n "$BATS_TEST_TMPDIR/install.sh"
	# A fresh shell: this one already sourced the unfilled script, whose constants are read-only.
	run bash -c 'PAR_INSTALL_SOURCED=1 && source "$1" && require_release && echo "$PAR_VERSION" &&
		asset_sha256 parcon-12-linux-amd64 && asset_sha256 par-runner-12.qcow2 &&
		asset_url parcon-12-linux-amd64' _ "$BATS_TEST_TMPDIR/install.sh"
	[ "$status" -eq 0 ]
	[ "${lines[0]}" = 12 ]
	[ "${lines[1]}" = "$(printf b | sha256sum | cut -d' ' -f1)" ]
	[ "${lines[2]}" = "$(printf a | sha256sum | cut -d' ' -f1)" ]
	[ "${lines[3]}" = \
		https://github.com/klponce/proxmox-actions-runners/releases/download/v12/parcon-12-linux-amd64 ]
}

@test "bad versions and checksum files are refused" {
	sums
	run "$FILL" v12 "$BATS_TEST_TMPDIR/sums"
	[ "$status" -eq 1 ]
	[[ $output == *"isn't a release number"* ]]
	run "$FILL" 0.1.0 "$BATS_TEST_TMPDIR/sums"
	[ "$status" -eq 1 ]
	run "$FILL" 012 "$BATS_TEST_TMPDIR/sums"
	[ "$status" -eq 1 ]

	echo 'abc  "$(reboot)"' >"$BATS_TEST_TMPDIR/bad"
	run "$FILL" 12 "$BATS_TEST_TMPDIR/bad"
	[ "$status" -eq 1 ]
	[[ $output == *"not a sha256sum line"* ]]

	: >"$BATS_TEST_TMPDIR/empty"
	run "$FILL" 12 "$BATS_TEST_TMPDIR/empty"
	[ "$status" -eq 1 ]
}
