#!/usr/bin/env bash
# Integration suite for proxmox-actions-runners against a throwaway Proxmox VE 9 node reached over SSH.
# Run it inside the dev container. See test/integration/README.md.
#
#   PAR_IT_SSH=root@<node> PAR_IT_THROWAWAY=1 test/integration/run.sh [setup|test|teardown|all]
set -euo pipefail

# Names of everything the suite creates on the node. They differ from a real install's (parzone, parnet,
# par-runners), so the suite can't touch one. The runner image gets its own pool: the controller prunes every
# template in its pool but the newest, so the test template and the image can't share one.
readonly POOL=par-it IMAGE_POOL=par-it-image ROLE=PARIntegration USER=par-it@pve TOKEN=it ZONE=parit VNET=paritnet

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
  PAR_IT_RUNNER_IMAGE    optional: a par-runner-<version>.qcow2 built from images/runner, with its .json manifest
                         next to it; the suite imports it and runs a worker from it
  PAR_IT_IMAGE_TEMPLATE_VMID  the runner image template's VMID (default 9001)
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
	RUNNER_IMAGE=${PAR_IT_RUNNER_IMAGE:-}
	IMAGE_TEMPLATE_VMID=${PAR_IT_IMAGE_TEMPLATE_VMID:-9001}
	STATE=${PAR_IT_STATE:-$ROOT/.agents/integration}
	if [[ -n $RUNNER_IMAGE ]]; then
		[[ -f $RUNNER_IMAGE ]] || die "PAR_IT_RUNNER_IMAGE: no such file: $RUNNER_IMAGE"
		IMAGE_MANIFEST=${RUNNER_IMAGE%.qcow2}.json
		[[ -f $IMAGE_MANIFEST ]] || die "no manifest next to the runner image: $IMAGE_MANIFEST"
	fi

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

# The controller's privileges, plus VM.GuestAgent.Unrestricted so the Go tests can look inside workers
# (proxmox.AgentExec). The controller itself never runs commands in a guest.
privileges() { (cd "$ROOT" && go run ./test/integration/privileges && echo VM.GuestAgent.Unrestricted) |
	paste -sd' ' -; }

preflight() {
	log "preflight on $HOST"
	local version
	version=$(node pveversion) || die "can't reach $PAR_IT_SSH over SSH"
	[[ $version == pve-manager/9.* ]] || die "the node runs $version; the project supports Proxmox VE 9.x only"
	node 'test ! -e /etc/pve/corosync.conf' || die "the node is in a cluster; the project supports standalone nodes only"
	node 'test -e /dev/kvm' || die "the node has no /dev/kvm; enable (nested) virtualization for it"
	echo "$version, standalone, KVM available"

	# Refuse VMIDs that belong to anything but the suite: the templates' and the 11 test VMIDs. This fails closed:
	# if the VM list can't be read or parsed, setup stops.
	local vms taken
	vms=$(node 'pvesh get /cluster/resources --type vm --output-format json') || die "can't list the node's VMs"
	taken=$(perl -MJSON -e '
		my ($pool, $image_pool, $tpl, $image_tpl, $first) = @ARGV;
		for my $vm (@{decode_json(do { local $/; <STDIN> })}) {
			my $id = $vm->{vmid} // next;
			my $in = $vm->{pool} // "";
			my $ok = $id == $tpl ? $in eq $pool
				: $id == $image_tpl ? $in eq $image_pool
				: $id >= $first && $id <= $first + 10 ? ($in eq $pool || $in eq $image_pool)
				: 1;
			print "$id " unless $ok;
		}' "$POOL" "$IMAGE_POOL" "$TEMPLATE_VMID" "$IMAGE_TEMPLATE_VMID" "$TEST_VMID" <<<"$vms") ||
		die "can't parse the node's VM list"
	[[ -z $taken ]] || die "VMIDs ${taken% } are in use by something other than the suite; set PAR_IT_TEMPLATE_VMID," \
		"PAR_IT_IMAGE_TEMPLATE_VMID, or PAR_IT_TEST_VMID"
	echo "VMIDs $TEMPLATE_VMID, $IMAGE_TEMPLATE_VMID, and $TEST_VMID-$((TEST_VMID + 10)) are free or the suite's own"
}

# acl_paths lists the paths the test role is granted on: the pools, the storage, and the VNet.
acl_paths() {
	printf '%s ' "/pool/$POOL"
	[[ -z $RUNNER_IMAGE ]] || printf '%s ' "/pool/$IMAGE_POOL"
	printf '%s %s' "/storage/$STORAGE" "/sdn/zones/$ZONE/$VNET"
}

# latest_runner_version prints the latest actions/runner release from GitHub's public API, such as 2.337.0.
latest_runner_version() {
	curl -fsSL -H 'Accept: application/vnd.github+json' https://api.github.com/repos/actions/runner/releases/latest |
		perl -MJSON -e 'my $tag = decode_json(do { local $/; <STDIN> })->{tag_name} // ""; $tag =~ s/^v//;
			$tag =~ /^\d+\.\d+\.\d+$/ or die "unexpected tag: $tag\n"; print $tag'
}

cmd_setup() {
	require_throwaway
	preflight

	log "fixtures"
	node_script fixtures.sh "ZONE=$ZONE" "VNET=$VNET" "POOL=$POOL" "ROLE=$ROLE" "USER=$USER" "PRIVS=$(privileges)"
	if [[ -n $RUNNER_IMAGE ]]; then
		node "pveum pool list --output-format json | grep -q '\"poolid\":\"$IMAGE_POOL\"' ||
			pveum pool add '$IMAGE_POOL' --comment 'proxmox-actions-runners integration suite: runner image'"
		echo "pool $IMAGE_POOL"
	fi

	# A token's secret is only shown when it's created, so setup always makes a new one. It goes straight into a
	# private file and is never printed.
	# Removing a token that still has ACLs leaves them behind for Proxmox to clean up with warnings, so drop them first.
	log "API token $USER!$TOKEN"
	node "for path in $(acl_paths); do
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
	node "for path in $(acl_paths); do
		pveum acl modify \"\$path\" --users '$USER' --tokens '$USER!$TOKEN' --roles '$ROLE' && echo \"\$path\"
	done"

	log "template $TEMPLATE_VMID"
	node_script template.sh "VMID=$TEMPLATE_VMID" "POOL=$POOL" "STORAGE=$STORAGE" "BRIDGE=$BUILD_BRIDGE" \
		"VNET=$VNET" "REBUILD=${PAR_IT_REBUILD_TEMPLATE:-0}"

	# parcon check template compares the template's runner version with the latest release, so tag the test template
	# with the latest one for a result that doesn't depend on when the suite runs.
	# Keep the template's version (par-tv, its creation time) and replace only the runner version.
	local runner version
	runner=$(latest_runner_version) || die "can't look up the latest actions/runner release"
	version=$(node "qm config $TEMPLATE_VMID" | sed -n 's/^tags: .*par-tv-\([0-9]*\).*/\1/p')
	[[ -n $version ]] || die "template $TEMPLATE_VMID has no par-tv tag; rebuild it with PAR_IT_REBUILD_TEMPLATE=1"
	node "qm set $TEMPLATE_VMID --tags 'par-managed;par-template;par-tv-$version;par-rv-$runner' >/dev/null"
	echo "tagged with actions/runner $runner, the latest release"

	[[ -z $RUNNER_IMAGE ]] || import_runner_image

	write_state
}

# import_runner_image copies the runner image to the node and imports it as a template in its own pool, tagged from
# its manifest the way the installer will.
import_runner_image() {
	log "runner image $(basename "$RUNNER_IMAGE") as template $IMAGE_TEMPLATE_VMID"
	local tags remote=/var/lib/vz/import/par-it-runner.qcow2 sum
	tags=$(perl -MJSON -e '
		my $m = decode_json(do { local $/; <STDIN> });
		my ($b) = grep { $_->{packer_run_uuid} eq $m->{last_run_uuid} } @{$m->{builds}};
		$b //= $m->{builds}[-1];
		my $rv = $b->{custom_data}{runner_version} or die "no runner_version in the manifest\n";
		print "par-managed;par-template;par-tv-$b->{build_time};par-rv-$rv";' <"$IMAGE_MANIFEST") ||
		die "can't read $IMAGE_MANIFEST"
	echo "tags: $tags"

	sum=$(sha256sum "$RUNNER_IMAGE" | cut -d' ' -f1)
	if [[ $(node "sha256sum $remote 2>/dev/null | cut -d' ' -f1") != "$sum" ]]; then
		echo "copying $(du -h "$RUNNER_IMAGE" | cut -f1) to the node"
		scp -q "${SSH_OPTS[@]}" "$RUNNER_IMAGE" "$PAR_IT_SSH:$remote.part"
		node "echo '$sum  $remote.part' | sha256sum -c --quiet - && mv $remote.part $remote" ||
			die "the runner image arrived corrupted"
	fi
	node_script import-image.sh "VMID=$IMAGE_TEMPLATE_VMID" "POOL=$IMAGE_POOL" "STORAGE=$STORAGE" "VNET=$VNET" \
		"IMAGE=$remote" "TAGS=$tags"
	IMAGE_RUNNER=${tags##*par-rv-}
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
	if [[ -n $RUNNER_IMAGE ]]; then
		cat >>"$STATE/env" <<EOF
PAR_PVE_IMAGE_POOL=$IMAGE_POOL
PAR_PVE_IMAGE_TEMPLATE_VMID=$IMAGE_TEMPLATE_VMID
PAR_IT_IMAGE_RUNNER=$IMAGE_RUNNER
EOF
		write_config "$IMAGE_POOL" "$name" "$fingerprint" >"$STATE/config-image.yaml"
	else
		rm -f "$STATE/config-image.yaml"
	fi
	write_config "$POOL" "$name" "$fingerprint" >"$STATE/config.yaml"
	echo "state written to $STATE"
}

# write_config prints a controller config for the parcon checks, for one pool. It names no GitHub App, like the
# config the installer writes before it creates one: check proxmox and check template don't need it.
write_config() {
	local pool=$1 name=$2 fingerprint=$3
	cat <<EOF
proxmox:
  url: $API_URL
  tokenId: $USER!$TOKEN
  tokenSecretFile: $STATE/token
  tlsFingerprint: "$fingerprint"
  node: $name
  pool: $pool
  storage: $STORAGE
  zone: $ZONE
  vnet: $VNET
  vmidRange: { start: $((TEST_VMID + 1)), end: $((TEST_VMID + 10)) }
github:
  configUrl: https://github.com/my-org/my-repo
worker:
  memoryMiB: 2048
scaleSets:
  - { name: par-integration, maxRunners: 1 }
EOF
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

	# The test template is tagged with the latest actions/runner release, so this is up to date. The release lookup
	# goes to the real github.com.
	log "parcon check template"
	local out
	out=$("$STATE/parcon" check template -config "$STATE/config.yaml" 2>&1) || {
		printf '%s\n' "$out"
		die "parcon check template failed"
	}
	printf '%s\n' "$out"
	grep -q "^ok    runner template $TEMPLATE_VMID, " <<<"$out" || die "check template didn't find template $TEMPLATE_VMID"
	grep -q "is the latest release" <<<"$out" || die "check template doesn't report the test template as up to date"

	if [[ -f $STATE/config-image.yaml ]]; then
		# The image's runner may be older than the latest release, which the check reports; that depends on when the
		# image was built, so only the template and its version are required here.
		log "parcon check template on the runner image"
		out=$("$STATE/parcon" check template -config "$STATE/config-image.yaml" 2>&1) || true
		printf '%s\n' "$out"
		grep -q "^ok    runner template $PAR_PVE_IMAGE_TEMPLATE_VMID, .*actions/runner $PAR_IT_IMAGE_RUNNER\$" <<<"$out" ||
			die "check template didn't report the runner image template with actions/runner $PAR_IT_IMAGE_RUNNER"
	fi

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
	# Always both pools: the image pool may be left from an earlier run with PAR_IT_RUNNER_IMAGE.
	node_script teardown.sh "ZONE=$ZONE" "VNET=$VNET" "POOLS=$POOL $IMAGE_POOL" "ROLE=$ROLE" "USER=$USER"
	rm -f "$STATE/env" "$STATE/token" "$STATE/config.yaml" "$STATE/config-image.yaml" "$STATE/github-app.pem" \
		"$STATE/parcon"
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
