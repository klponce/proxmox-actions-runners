#!/usr/bin/env bats
# Arguments, the answers file, settings validation, and the IPv4 helpers.

load helpers

@test "parse_args takes a command and flags" {
	parse_args uninstall --answers par.env --yes --dry-run
	[ "$COMMAND" = uninstall ]
	[ "$ANSWERS" = par.env ]
	[ "$ASSUME_YES" = 1 ]
	[ "$DRY_RUN" = 1 ]
}

@test "parse_args defaults to install and rejects unknown arguments" {
	parse_args
	[ "$COMMAND" = install ]
	run parse_args --version 1.0
	[ "$status" -eq 2 ]
	[[ $output == *usage:* ]]
}

@test "the script refuses to install from the source tree" {
	run require_release
	[ "$status" -eq 1 ]
	[[ $output == *"from the source tree"* ]]
	PAR_VERSION=0.1.0
	run require_release
	[ "$status" -eq 0 ]
}

@test "load_answers sets known keys literally" {
	cat >"$BATS_TEST_TMPDIR/par.env" <<'EOF'
# a comment
PAR_GITHUB_URL=https://github.com/my-org/my-repo

PAR_MAX_RUNNERS="4"
PAR_SCALE_SET=$(touch /tmp/pwned)
EOF
	load_answers "$BATS_TEST_TMPDIR/par.env"
	[ "$PAR_GITHUB_URL" = https://github.com/my-org/my-repo ]
	[ "$PAR_MAX_RUNNERS" = 4 ]
	[ "$PAR_SCALE_SET" = '$(touch /tmp/pwned)' ]
	apply_defaults
	run validate_settings
	[ "$status" -eq 1 ]
	[[ $output == *PAR_SCALE_SET* ]]
}

@test "load_answers rejects unknown keys and malformed lines" {
	echo "PAR_TYPO=1" >"$BATS_TEST_TMPDIR/a.env"
	run load_answers "$BATS_TEST_TMPDIR/a.env"
	[ "$status" -eq 1 ]
	[[ $output == *"unknown setting PAR_TYPO"* ]]
	echo "just words" >"$BATS_TEST_TMPDIR/b.env"
	run load_answers "$BATS_TEST_TMPDIR/b.env"
	[ "$status" -eq 1 ]
	[[ $output == *"not KEY=value"* ]]
}

@test "the defaults are valid for an organization" {
	settings_ok
	run validate_settings
	[ "$status" -eq 0 ]
	[ "$PAR_LABELS" = proxmox-ubuntu-26.04 ]
}

@test "validate_settings reports every bad setting" {
	PAR_GITHUB_URL=https://gitlab.com/x
	PAR_MIN_RUNNERS=3
	PAR_MAX_RUNNERS=2
	PAR_GATEWAY_IP=192.0.2.10/24
	PAR_VLAN=5000
	PAR_VMID_START=10000
	PAR_VMID_END=10003
	apply_defaults
	run validate_settings
	[ "$status" -eq 1 ]
	[[ $output == *PAR_GITHUB_URL* ]]
	[[ $output == *PAR_MIN_RUNNERS* ]]
	[[ $output == *PAR_LAN_GATEWAY* ]]
	[[ $output == *PAR_VLAN* ]]
	[[ $output == *"VMID range"* ]]
}

@test "repository runners must use the default runner group" {
	PAR_GITHUB_URL=https://github.com/my-org/my-repo
	PAR_RUNNER_GROUP=builders
	apply_defaults
	run validate_settings
	[ "$status" -eq 1 ]
	[[ $output == *"runner group default"* ]]
}

@test "a static address needs the LAN router" {
	settings_ok
	PAR_CONTROLLER_IP=192.0.2.20/24
	PAR_LAN_GATEWAY=192.0.2.1
	run validate_settings
	[ "$status" -eq 0 ]
}

@test "IPv4 helpers" {
	is_ipv4 192.0.2.1
	run ! is_ipv4 192.0.2.256
	run ! is_ipv4 192.0.2
	is_cidr 10.251.0.0/22
	run ! is_cidr 10.251.0.0/33
	run ! is_cidr 10.251.0.0
	cidrs_overlap 10.251.0.0/22 10.251.3.7
	cidrs_overlap 10.0.0.0/8 10.251.0.0/22
	run ! cidrs_overlap 10.251.0.0/22 10.251.4.0/24
	[ "$(network_of 192.0.2.10/24)" = 192.0.2.0/24 ]
	[ "$(network_of 10.251.3.7/22)" = 10.251.0.0/22 ]
}
