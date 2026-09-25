#!/usr/bin/env bats
# bump-runner.sh, with curl, git, and gh stubbed and the pin file copied.

bats_require_minimum_version 1.5.0

REPO="$BATS_TEST_DIRNAME/../../.."
NEW_SUM=$(printf new | sha256sum | cut -d' ' -f1)

setup() {
	STUBS="$BATS_TEST_TMPDIR/bin"
	CALLS="$BATS_TEST_TMPDIR/calls"
	mkdir -p "$STUBS"
	: >"$CALLS"
	export PATH="$STUBS:$PATH" CALLS
	cp "$REPO/images/runner/runner.pkr.hcl" "$BATS_TEST_TMPDIR/runner.pkr.hcl"
	export PIN_FILE="$BATS_TEST_TMPDIR/runner.pkr.hcl"
	BUMP_RUNNER_SOURCED=1
	# shellcheck source=/dev/null
	source "$REPO/.github/scripts/bump-runner.sh"
	set +u
	stub git ''
	stub gh ''
}

stub() {
	printf '#!/usr/bin/env bash\necho "%s $*" >>"$CALLS"\n%s\n' "$1" "$2" >"$STUBS/$1"
	chmod +x "$STUBS/$1"
}

# release VERSION: GitHub's latest actions/runner release is VERSION.
release() {
	cat >"$BATS_TEST_TMPDIR/release.json" <<EOF
{"tag_name":"v$1","assets":[
  {"name":"actions-runner-osx-x64-$1.tar.gz","digest":"sha256:$(printf osx | sha256sum | cut -d' ' -f1)"},
  {"name":"actions-runner-linux-x64-$1.tar.gz","digest":"sha256:$NEW_SUM"}]}
EOF
	stub curl "cat '$BATS_TEST_TMPDIR/release.json'"
}

@test "pinned and pin read and write only their own variable" {
	[ "$(pinned runner_version)" = 2.337.0 ]
	pin runner_version 2.338.0
	pin runner_sha256 "$NEW_SUM"
	[ "$(pinned runner_version)" = 2.338.0 ]
	[ "$(pinned runner_sha256)" = "$NEW_SUM" ]
	[ "$(pinned output_directory)" = output-runner ]
	grep -q 'description = "actions/runner release to install' "$PIN_FILE"
}

@test "latest_release reads the linux archive's digest" {
	release 2.338.0
	[ "$(latest_release)" = "2.338.0 $NEW_SUM" ]
}

@test "nothing happens when the pin is current" {
	release 2.337.0
	run main
	[ "$status" -eq 0 ]
	run ! grep -q '^\(git\|gh\) ' "$CALLS"
	[ "$(pinned runner_version)" = 2.337.0 ]
}

@test "a newer release opens a pull request with the new pin" {
	release 2.338.0
	stub git 'case "$1" in ls-remote) exit 2 ;; esac'
	run main
	[ "$status" -eq 0 ]
	[ "$(pinned runner_version)" = 2.338.0 ]
	[ "$(pinned runner_sha256)" = "$NEW_SUM" ]
	grep -q '^git switch -c bump/actions-runner-2.338.0$' "$CALLS"
	grep -q '^gh pr create --base main --head bump/actions-runner-2.338.0' "$CALLS"
}

@test "an existing branch means the pull request exists" {
	release 2.338.0
	stub git 'exit 0'
	run main
	[ "$status" -eq 0 ]
	[[ $output == *"already open"* ]]
	[ "$(pinned runner_version)" = 2.337.0 ]
	run ! grep -q '^gh ' "$CALLS"
}

@test "an older latest release leaves a newer pin alone" {
	release 2.336.0
	run main
	[[ $output == *"leaving it"* ]]
	[ "$(pinned runner_version)" = 2.337.0 ]
}
