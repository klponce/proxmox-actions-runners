#!/usr/bin/env bash
# Integration suite for proxmox-actions-runners against a throwaway Proxmox VE 9 node reached over SSH.
# Run it inside the dev container. See test/integration/README.md.
#
#   PAR_IT_SSH=root@<node> PAR_IT_THROWAWAY=1 test/integration/run.sh [setup|test|teardown|all]
set -euo pipefail

# Names of everything the suite creates on the node. They differ from a real install's (parzone, parnet,
# par-runners), so the suite can't touch one.
readonly POOL=par-it ROLE=PARIntegration USER=par-it@pve TOKEN=it ZONE=parit VNET=paritnet

HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
ROOT=$(cd "$HERE/../.." && pwd)
readonly HERE ROOT

usage() {
	cat <<'EOF'
usage: test/integration/run.sh [setup|test|teardown|all]

  setup     create the suite's objects on the node (idempotent); needs PAR_IT_THROWAWAY=1
  test      run the integration tests and checks against them; needs PAR_IT_THROWAWAY=1
  teardown  remove everything setup created; needs PAR_IT_THROWAWAY=1
  all       setup, then test (the default)

environment:
  PAR_IT_SSH             root@<node>, required
  PAR_IT_SSH_KEY         SSH identity file (default: ssh's own defaults)
  PAR_IT_THROWAWAY       set to 1 to confirm the node is disposable; the suite changes it
  PAR_IT_API_URL         default https://<node>:8006/api2/json
  PAR_IT_STORAGE         VM disk storage (default local-lvm)
  PAR_IT_BUILD_BRIDGE    bridge with DHCP and internet for building the template (default vmbr0)
  PAR_IT_TEMPLATE_VMID   the test template's VMID (default 9000)
  PAR_IT_TEST_VMID       first of 11 VMIDs the tests create and destroy (default 9900)
  PAR_IT_REBUILD_TEMPLATE  set to 1 to rebuild an existing template
  PAR_IT_STATE           private working directory (default .agents/integration, which git ignores)
EOF
}

log() { printf '\n== %s\n' "$*" >&2; }
die() {
	printf 'error: %s\n' "$*" >&2
	exit 1
}

load_settings() {
	: "${PAR_IT_SSH:?set PAR_IT_SSH to root@<node>; see test/integration/run.sh -h}"
	HOST=${PAR_IT_SSH#*@}
	API_URL=${PAR_IT_API_URL:-https://$HOST:8006/api2/json}
	STORAGE=${PAR_IT_STORAGE:-local-lvm}
	BUILD_BRIDGE=${PAR_IT_BUILD_BRIDGE:-vmbr0}
	TEMPLATE_VMID=${PAR_IT_TEMPLATE_VMID:-9000}
	TEST_VMID=${PAR_IT_TEST_VMID:-9900}
	STATE=${PAR_IT_STATE:-$ROOT/.agents/integration}

	umask 077
	mkdir -p "$STATE"
	chmod 700 "$STATE"
	SSH_OPTS=(-o BatchMode=yes -o ConnectTimeout=15 -o StrictHostKeyChecking=accept-new
		-o "UserKnownHostsFile=$STATE/known_hosts")
	if [[ -n ${PAR_IT_SSH_KEY:-} ]]; then
		SSH_OPTS+=(-i "$PAR_IT_SSH_KEY" -o IdentitiesOnly=yes)
	fi
}

require_throwaway() {
	[[ ${PAR_IT_THROWAWAY:-} == 1 ]] ||
		die "the suite changes the node; set PAR_IT_THROWAWAY=1 to confirm $HOST is a disposable test node"
}

# node runs a shell command on the node as root. Callers build the command locally, on purpose.
# shellcheck disable=SC2029
node() { ssh "${SSH_OPTS[@]}" "$PAR_IT_SSH" "$@"; }

# node_script runs test/integration/node/<script> on the node with the given NAME=value environment.
node_script() {
	local script=$1 kv
	shift
	local env=()
	for kv in "$@"; do
		env+=("$(printf '%q' "$kv")")
	done
	node "env ${env[*]} bash -s" <"$HERE/node/$script"
}

privileges() { (cd "$ROOT" && go run ./test/integration/privileges) | paste -sd' ' -; }

preflight() {
	log "preflight on $HOST"
	local version
	version=$(node pveversion) || die "can't reach $PAR_IT_SSH over SSH"
	[[ $version == pve-manager/9.* ]] || die "the node runs $version; the project supports Proxmox VE 9.x only"
	node 'test ! -e /etc/pve/corosync.conf' || die "the node is in a cluster; the project supports standalone nodes only"
	node 'test -e /dev/kvm' || die "the node has no /dev/kvm; enable (nested) virtualization for it"
	echo "$version, standalone, KVM available"

	# Refuse VMIDs that belong to anything but the suite: the template's and the 11 test VMIDs. This fails closed:
	# if the VM list can't be read or parsed, setup stops.
	local vms taken
	vms=$(node 'pvesh get /cluster/resources --type vm --output-format json') || die "can't list the node's VMs"
	taken=$(perl -MJSON -e '
		my ($pool, $tpl, $first) = @ARGV;
		for my $vm (@{decode_json(do { local $/; <STDIN> })}) {
			my $id = $vm->{vmid} // next;
			next unless $id == $tpl || ($id >= $first && $id <= $first + 10);
			print "$id " unless ($vm->{pool} // "") eq $pool;
		}' "$POOL" "$TEMPLATE_VMID" "$TEST_VMID" <<<"$vms") || die "can't parse the node's VM list"
	[[ -z $taken ]] || die "VMIDs ${taken% } are in use outside pool $POOL; set PAR_IT_TEMPLATE_VMID or PAR_IT_TEST_VMID"
	echo "VMIDs $TEMPLATE_VMID and $TEST_VMID-$((TEST_VMID + 10)) are free or the suite's own"
}

cmd_setup() {
	require_throwaway
	preflight

	log "fixtures"
	node_script fixtures.sh "ZONE=$ZONE" "VNET=$VNET" "POOL=$POOL" "ROLE=$ROLE" "USER=$USER" "PRIVS=$(privileges)"

	# A token's secret is only shown when it's created, so setup always makes a new one. It goes straight into a
	# private file and is never printed.
	# Removing a token that still has ACLs leaves them behind for Proxmox to clean up with warnings, so drop them first.
	log "API token $USER!$TOKEN"
	node "for path in /pool/$POOL /storage/$STORAGE /sdn/zones/$ZONE/$VNET; do
			pveum acl delete \"\$path\" --tokens '$USER!$TOKEN' --roles '$ROLE' >/dev/null 2>&1 || true
		done
		pveum user token remove '$USER' '$TOKEN' >/dev/null 2>&1 || true
		pveum user token add '$USER' '$TOKEN' --privsep 1 --comment 'proxmox-actions-runners integration suite' \
			--output-format json" |
		perl -MJSON -e '
			my $d = decode_json(do { local $/; <STDIN> });
			die "no token secret in the response\n" unless length($d->{value} // "");
			open my $f, ">", $ARGV[0] or die "open $ARGV[0]: $!\n"; print $f "$d->{value}\n"; close $f;
			chmod 0600, $ARGV[0]; print STDERR "secret saved for $d->{q(full-tokenid)}\n";' "$STATE/token" ||
		die "creating the API token failed"

	# A privilege-separated token gets only what both it and its user have, so both get every ACL.
	log "ACLs"
	node "for path in /pool/$POOL /storage/$STORAGE /sdn/zones/$ZONE/$VNET; do
		pveum acl modify \"\$path\" --users '$USER' --tokens '$USER!$TOKEN' --roles '$ROLE' && echo \"\$path\"
	done"

	log "template $TEMPLATE_VMID"
	node_script template.sh "VMID=$TEMPLATE_VMID" "POOL=$POOL" "STORAGE=$STORAGE" "BRIDGE=$BUILD_BRIDGE" \
		"VNET=$VNET" "REBUILD=${PAR_IT_REBUILD_TEMPLATE:-0}"

	write_state
}

write_state() {
	local name fingerprint
	name=$(node hostname)
	fingerprint=$(node 'pvenode cert info --output-format json' |
		perl -MJSON -0777 -ne 'print map { $_->{fingerprint} } grep { $_->{filename} eq "pve-ssl.pem" } @{decode_json($_)}')
	[[ -n $fingerprint ]] || die "couldn't read the node's pve-ssl.pem fingerprint"

	# The same variables the Go integration tests read. The secret stays in $STATE/token.
	cat >"$STATE/env" <<EOF
PAR_PVE_URL=$API_URL
PAR_PVE_TOKEN_ID=$USER!$TOKEN
PAR_PVE_FINGERPRINT=$fingerprint
PAR_PVE_NODE=$name
PAR_PVE_STORAGE=$STORAGE
PAR_PVE_POOL=$POOL
PAR_PVE_VNET=$VNET
PAR_PVE_TEMPLATE_VMID=$TEMPLATE_VMID
PAR_PVE_TEST_VMID=$TEST_VMID
EOF

	# A controller config for parcon check proxmox. The GitHub section is a placeholder that check proxmox ignores.
	cat >"$STATE/config.yaml" <<EOF
proxmox:
  url: $API_URL
  tokenId: $USER!$TOKEN
  tokenSecretFile: $STATE/token
  tlsFingerprint: "$fingerprint"
  node: $name
  pool: $POOL
  storage: $STORAGE
  zone: $ZONE
  vnet: $VNET
  vmidRange: { start: $((TEST_VMID + 1)), end: $((TEST_VMID + 10)) }
github:
  configUrl: https://github.com/my-org/my-repo
  app: { clientId: Iv23liEXAMPLE0000000, installationId: 1, privateKeyFile: $STATE/github-app.pem }
worker:
  memoryMiB: 2048
scaleSets:
  - { name: par-integration, maxRunners: 1 }
EOF
	echo "state written to $STATE"
}

# check_proxmox runs parcon check proxmox against the suite's config and prints its output.
check_proxmox() { "$STATE/parcon" check proxmox -config "$STATE/config.yaml" 2>&1; }

# expect_check_failure runs parcon check proxmox and fails unless it fails with a line containing want.
expect_check_failure() {
	local want=$1 out
	if out=$(check_proxmox); then
		printf '%s\n' "$out"
		die "parcon check proxmox passed; expected it to fail with: $want"
	fi
	grep -qF -- "$want" <<<"$out" || {
		printf '%s\n' "$out"
		die "parcon check proxmox failed without: $want"
	}
	echo "failed as expected: $want"
}

restore_access() {
	node "pveum role modify '$ROLE' --privs '$(privileges)' &&
		pveum acl modify /sdn/zones/$ZONE/$VNET --users '$USER' --roles '$ROLE'" ||
		echo "warning: restoring the test role and ACL failed; rerun setup" >&2
}

cmd_test() {
	require_throwaway
	[[ -f $STATE/env && -f $STATE/token && -f $STATE/config.yaml ]] || die "no state in $STATE; run setup first"
	set -a
	# shellcheck source=/dev/null
	. "$STATE/env"
	set +a
	# The Go tests read the secret from the environment, so it never appears on a command line.
	PAR_PVE_TOKEN_SECRET=$(<"$STATE/token")
	export PAR_PVE_TOKEN_SECRET

	log "Go integration tests"
	(cd "$ROOT" && go test -tags integration -count=1 -timeout 30m -run Integration -v \
		./internal/proxmox/ ./internal/controller/) || die "Go integration tests failed"

	log "parcon check proxmox"
	(cd "$ROOT" && go build -o "$STATE/parcon" ./cmd/parcon)
	check_proxmox || die "parcon check proxmox failed"

	# The next checks break the test role on purpose; the trap puts it back even if one fails.
	trap restore_access EXIT

	log "check proxmox without Pool.Audit (the token can't see VM pools)"
	node "pveum role modify '$ROLE' --privs '$(privileges | tr ' ' '\n' | grep -vx Pool.Audit | paste -sd' ' -)'"
	expect_check_failure "missing Pool.Audit"
	restore_access

	log "check proxmox with the VNet ACL on the token only (not its user)"
	node "pveum acl delete /sdn/zones/$ZONE/$VNET --users '$USER' --roles '$ROLE'"
	expect_check_failure "grant it to both the token and its user"
	restore_access

	trap - EXIT
	check_proxmox >/dev/null || die "parcon check proxmox fails after restoring the test role"
	log "all integration tests passed"
}

cmd_teardown() {
	require_throwaway
	log "teardown on $HOST"
	node_script teardown.sh "ZONE=$ZONE" "VNET=$VNET" "POOL=$POOL" "ROLE=$ROLE" "USER=$USER"
	rm -f "$STATE/env" "$STATE/token" "$STATE/config.yaml" "$STATE/parcon"
}

main() {
	local cmd=${1:-all}
	case $cmd in
	-h | --help | help)
		usage
		return
		;;
	setup | test | teardown | all) ;;
	*)
		usage >&2
		exit 2
		;;
	esac
	load_settings
	case $cmd in
	setup) cmd_setup ;;
	test) cmd_test ;;
	teardown) cmd_teardown ;;
	all)
		cmd_setup
		cmd_test
		;;
	esac
}

main "$@"
