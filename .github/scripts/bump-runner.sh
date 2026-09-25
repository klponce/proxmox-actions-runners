#!/usr/bin/env bash
# bump-runner.sh checks for a newer actions/runner release than the runner image pins, and opens a pull request that
# updates the pin (images/runner/runner.pkr.hcl) when there is one. The scheduled workflow
# .github/workflows/runner-release.yml runs it with GH_TOKEN set. GitHub stops accepting a runner that doesn't update
# itself 30 days after a newer release, so each one has to ship in a project release within that time.
set -euo pipefail

PIN_FILE=${PIN_FILE:-images/runner/runner.pkr.hcl} # tests point it at a copy

# pinned VARIABLE prints the default of a Packer variable in PIN_FILE.
pinned() {
	sed -n "/^variable \"$1\" {/,/^}/ s/^ *default *= *\"\\([^\"]*\\)\".*/\\1/p" "$PIN_FILE"
}

# pin VARIABLE VALUE sets the default of a Packer variable in PIN_FILE.
pin() {
	sed -i "/^variable \"$1\" {/,/^}/ s/^\\( *default *= *\\)\"[^\"]*\"/\\1\"$2\"/" "$PIN_FILE"
}

# latest_release prints the latest actions/runner version and the SHA-256 of its linux-x64 archive, from GitHub's
# asset digest.
latest_release() {
	curl -fsSL -H 'Accept: application/vnd.github+json' https://api.github.com/repos/actions/runner/releases/latest |
		perl -MJSON -e '
			my $d = decode_json(do { local $/; <STDIN> });
			(my $v = $d->{tag_name}) =~ s/^v//;
			$v =~ /^\d+\.\d+\.\d+$/ or die "unexpected tag $d->{tag_name}\n";
			my ($asset) = grep { $_->{name} eq "actions-runner-linux-x64-$v.tar.gz" } @{$d->{assets}};
			my ($sum) = ($asset->{digest} // "") =~ /^sha256:([0-9a-f]{64})$/ or die "no SHA-256 for $v\n";
			print "$v $sum\n";'
}

main() {
	cd "$(dirname "${BASH_SOURCE[0]}")/../.."
	local current version sum branch
	current=$(pinned runner_version)
	read -r version sum <<<"$(latest_release)"
	echo "pinned actions/runner $current, latest $version"
	[[ $version != "$current" ]] || return 0
	[[ $(printf '%s\n' "$current" "$version" | sort -V | tail -n 1) == "$version" ]] || {
		echo "the pin is newer than the latest release; leaving it"
		return 0
	}

	branch=bump/actions-runner-$version
	if git ls-remote --exit-code --heads origin "$branch" >/dev/null; then
		echo "branch $branch exists; its pull request is already open or was closed on purpose"
		return 0
	fi
	pin runner_version "$version"
	pin runner_sha256 "$sum"
	git switch -c "$branch"
	git commit -q -am "chore(images): bump actions/runner to $version"
	git push -q origin "$branch"
	gh pr create --base main --head "$branch" --title "chore(images): bump actions/runner to $version" --body "\
actions/runner $version is out; the runner image pins $current. GitHub stops accepting a runner that doesn't update \
itself 30 days after a newer release, so this needs to ship in a project release before then.

After merging, tag a release. Opened by .github/workflows/runner-release.yml. Pull requests opened with the \
workflow's token don't start other workflows, so close and reopen this one to run CI."
}

[[ -n ${BUMP_RUNNER_SOURCED:-} ]] || main "$@"
