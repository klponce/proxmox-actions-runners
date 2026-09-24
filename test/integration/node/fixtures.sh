#!/usr/bin/env bash
# Runs on the test node as root, sent by run.sh. Creates the integration suite's SDN zone and VNet, pool, role, and
# user. Safe to re-run: existing objects are kept, and the role's privileges are reset to PRIVS.
#
# Environment: ZONE VNET POOL ROLE USER PRIVS (space-separated privilege names).
set -euo pipefail

exists() { # exists <pveum list command> <json key> <value>
	"$1" "$2" list --output-format json | grep -q "\"$3\":\"$4\""
}

main() {
	: "${ZONE:?}" "${VNET:?}" "${POOL:?}" "${ROLE:?}" "${USER:?}" "${PRIVS:?}"
	local comment="proxmox-actions-runners integration suite"

	echo "== SDN zone $ZONE and VNet $VNET"
	pvesh get "/cluster/sdn/zones/$ZONE" >/dev/null 2>&1 || pvesh create /cluster/sdn/zones --type simple --zone "$ZONE"
	pvesh get "/cluster/sdn/vnets/$VNET" >/dev/null 2>&1 || pvesh create /cluster/sdn/vnets --vnet "$VNET" --zone "$ZONE"
	pvesh set /cluster/sdn >/dev/null
	for _ in $(seq 1 30); do
		ip link show "$VNET" >/dev/null 2>&1 && break
		sleep 1
	done
	ip link show "$VNET" >/dev/null

	echo "== pool $POOL"
	exists pveum pool poolid "$POOL" || pveum pool add "$POOL" --comment "$comment"

	echo "== role $ROLE"
	if exists pveum role roleid "$ROLE"; then
		pveum role modify "$ROLE" --privs "$PRIVS"
	else
		pveum role add "$ROLE" --privs "$PRIVS"
	fi

	echo "== user $USER"
	exists pveum user userid "$USER" || pveum user add "$USER" --comment "$comment"
}

main "$@"
