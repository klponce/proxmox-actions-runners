#!/usr/bin/env bash
# Runs on the test node as root, sent by run.sh. Prints the objects a real install owns on the node, one per line and
# sorted, so run.sh can compare before and after an install.sh command: the pools, role, user, SDN zone and VNet, and
# every VM in the install's pools. The names are install.sh's, which the suite's own objects never use.
#
# shellcheck disable=SC2016 # The Perl code is single-quoted on purpose.
set -euo pipefail

list() { # list <pvesh or pveum command...> -- <perl expression over the decoded JSON array in @$d>
	local args=() expr
	while [[ $1 != -- ]]; do
		args+=("$1")
		shift
	done
	expr=$2
	"${args[@]}" --output-format json | perl -MJSON -e "my \$d = decode_json(do { local \$/; <STDIN> }); $expr"
}

main() {
	{
		list pvesh get /pools -- 'print "pool $_->{poolid}\n" for grep { $_->{poolid} =~ /^par-(system|runners)$/ } @$d'
		list pveum role list -- 'print "role $_->{roleid}\n" for grep { $_->{roleid} eq "PARController" } @$d'
		list pveum user list -- 'print "user $_->{userid}\n" for grep { $_->{userid} eq "par\@pve" } @$d'
		list pvesh get /cluster/sdn/zones -- 'print "zone $_->{zone}\n" for grep { $_->{zone} eq "parzone" } @$d'
		list pvesh get /cluster/sdn/vnets -- 'print "vnet $_->{vnet}\n" for grep { $_->{vnet} eq "parnet" } @$d'
		list pvesh get /cluster/resources --type vm -- '
			print "vm $_->{vmid} $_->{name}\n" for grep { ($_->{pool} // "") =~ /^par-(system|runners)$/ } @$d'
	} | sort
}

main "$@"
