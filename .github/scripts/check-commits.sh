#!/usr/bin/env bash
# check-commits.sh BASE HEAD checks that every commit in BASE..HEAD, merges aside, has a Conventional Commits
# subject: `type(scope)!: description`, with one of the types below. release-please reads these subjects to choose the
# next version and write the changelog, so a commit it can't read is left out of both. The commits workflow runs it on
# every pull request.
set -euo pipefail

# feat makes a minor release and fix and perf a patch release (breaking changes, marked with ! or a BREAKING CHANGE
# footer, a minor one before 1.0). The rest ship with the next release without making one.
readonly TYPES="feat|fix|perf|revert|docs|refactor|test|build|ci|chore"
readonly SUBJECT="^($TYPES)(\\([a-z0-9._/-]+\\))?!?: [^ ].*[^.]\$"

main() {
	local base=${1:?usage: check-commits.sh BASE HEAD} head=${2:?usage: check-commits.sh BASE HEAD}
	local sha subject bad=0
	while IFS=' ' read -r sha subject; do
		[[ -n $sha ]] || continue
		if [[ ! $subject =~ $SUBJECT ]]; then
			echo "${sha:0:12}: \"$subject\" isn't a Conventional Commits subject" >&2
			bad=1
		fi
	done < <(git log --no-merges --format='%H %s' "$base..$head")
	if ((bad)); then
		echo "Write subjects as type(scope): description, with type one of ${TYPES//|/, }, and no period at" \
			"the end; see AGENTS.md, \"Commits and releases\". Reword them with git rebase." >&2
		return 1
	fi
	echo "every commit in $base..$head has a Conventional Commits subject"
}

[[ -n ${CHECK_COMMITS_SOURCED:-} ]] || main "$@"
