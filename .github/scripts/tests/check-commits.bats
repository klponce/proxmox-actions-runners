#!/usr/bin/env bats
# check-commits.sh, against a throwaway repository.

bats_require_minimum_version 1.5.0 # for `run !`

CHECK="$BATS_TEST_DIRNAME/../check-commits.sh"

setup() {
	cd "$BATS_TEST_TMPDIR"
	git init -q -b main repo
	cd repo
	git config user.name test
	git config user.email test@example.com
	git commit -q --allow-empty -m "chore: start"
	git switch -q -c topic
}

commit() { git commit -q --allow-empty -m "$1"; }

@test "Conventional Commits subjects pass" {
	commit "feat: parcon status"
	commit "fix(pvecli): take a guest command's status"
	commit "feat(config)!: rename a key"
	commit "chore(images): update actions/runner to 2.339.0"
	commit "docs: say how releases work"
	run "$CHECK" main HEAD
	[ "$status" -eq 0 ]
}

@test "other subjects fail, each named" {
	commit "feat: fine"
	commit "Add a thing"
	commit "feature: wrong type"
	commit "fix: ends with a period."
	commit "fix:no space"
	commit "Fix(pvecli): capital type"
	run ! "$CHECK" main HEAD
	[[ $output == *'"Add a thing"'* ]]
	[[ $output == *'"feature: wrong type"'* ]]
	[[ $output == *'"fix: ends with a period."'* ]]
	[[ $output == *'"fix:no space"'* ]]
	[[ $output == *'"Fix(pvecli): capital type"'* ]]
	[[ $output != *'"feat: fine"'* ]]
}

@test "merge commits are skipped" {
	commit "feat: on the topic"
	git switch -q main
	commit "fix: on main"
	git merge -q --no-ff -m "Merge pull request #1 from someone/topic" topic
	git switch -q -c next
	run "$CHECK" main~2 HEAD
	[ "$status" -eq 0 ]
}
