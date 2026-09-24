#!/usr/bin/env bash
# Runs on the test node as root, sent by run.sh. Removes everything the integration suite created: every VM in its
# pool, the pool, the user (with its token and ACLs), the role, the SDN zone and VNet, the template snippet and
# image, and the snippets content type it added to the local storage. Missing objects are skipped.
#
# Environment: ZONE VNET POOL ROLE USER
set -euo pipefail

readonly IMG=/var/lib/vz/import/ubuntu-26.04-server-cloudimg-amd64.img
readonly SNIPPET=/var/lib/vz/snippets/par-it-agent.yaml
readonly CONTENT_MARKER=/root/.par-it-local-content

main() {
	: "${ZONE:?}" "${VNET:?}" "${POOL:?}" "${ROLE:?}" "${USER:?}"

	if pvesh get "/pools/$POOL" >/dev/null 2>&1; then
		local vmid
		for vmid in $(pvesh get "/pools/$POOL" --output-format json |
			perl -MJSON -0777 -ne 'print "$_->{vmid}\n" for grep { $_->{type} eq "qemu" } @{decode_json($_)->{members}}'); do
			echo "== destroy VM $vmid"
			qm stop "$vmid" >/dev/null 2>&1 || true
			qm destroy "$vmid" --purge 1
		done
		echo "== pool $POOL"
		pveum pool delete "$POOL"
	fi

	if pveum user list --output-format json | grep -q "\"userid\":\"$USER\""; then
		# Remove the user's and its tokens' ACLs first: removing a token or user leaves them behind for Proxmox to
		# clean up with warnings.
		local path type ugid role
		while read -r path type ugid role; do
			echo "== ACL $path for $ugid"
			pveum acl delete "$path" "--${type}s" "$ugid" --roles "$role"
		done < <(pveum acl list --output-format json | perl -MJSON -e '
			my $user = $ARGV[0];
			for my $acl (@{decode_json(do { local $/; <STDIN> })}) {
				next unless $acl->{ugid} eq $user || index($acl->{ugid}, "$user!") == 0;
				print join(" ", @{$acl}{qw(path type ugid roleid)}), "\n";
			}' "$USER")

		local token
		for token in $(pveum user token list "$USER" --output-format json |
			perl -MJSON -0777 -ne 'print "$_->{tokenid}\n" for @{decode_json($_)}'); do
			echo "== token $USER!$token"
			pveum user token remove "$USER" "$token"
		done
		echo "== user $USER"
		pveum user delete "$USER"
	fi
	if pveum role list --output-format json | grep -q "\"roleid\":\"$ROLE\""; then
		echo "== role $ROLE"
		pveum role delete "$ROLE"
	fi

	local changed=0
	if pvesh get "/cluster/sdn/vnets/$VNET" >/dev/null 2>&1; then
		echo "== SDN VNet $VNET"
		pvesh delete "/cluster/sdn/vnets/$VNET"
		changed=1
	fi
	if pvesh get "/cluster/sdn/zones/$ZONE" >/dev/null 2>&1; then
		echo "== SDN zone $ZONE"
		pvesh delete "/cluster/sdn/zones/$ZONE"
		changed=1
	fi
	[[ $changed == 0 ]] || pvesh set /cluster/sdn >/dev/null

	rm -f "$SNIPPET" "$IMG"
	rmdir /var/lib/vz/snippets 2>/dev/null || true # only if the suite's snippet was the last one
	if [[ -e $CONTENT_MARKER ]]; then
		echo "== restore local storage content: $(cat "$CONTENT_MARKER")"
		pvesm set local --content "$(cat "$CONTENT_MARKER")"
		rm -f "$CONTENT_MARKER"
	fi
	echo "== done"
}

main "$@"
