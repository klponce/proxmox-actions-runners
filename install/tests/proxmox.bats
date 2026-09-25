#!/usr/bin/env bats
# Proxmox queries, the guest agent, preflight checks, and the controller config, against stubbed tools.

load helpers

# resources stubs pvesh so /cluster/resources returns the given JSON.
resources() {
	cat >"$BATS_TEST_TMPDIR/resources.json"
	stub pvesh "case \"\$*\" in
		'get /cluster/resources'*) cat '$BATS_TEST_TMPDIR/resources.json' ;;
		'get /nodes --output-format json') echo '[{\"node\":\"pve1\"}]' ;;
		*) exit 1 ;;
	esac"
}

@test "vms_with_tag finds tagged VMs in one pool" {
	resources <<'EOF'
[{"vmid":101,"pool":"par-system","tags":"par-managed;par-gateway"},
 {"vmid":102,"pool":"par-system","tags":"par-managed;par-controller;par-release-0.1.0"},
 {"vmid":103,"pool":"other","tags":"par-managed;par-controller"},
 {"vmid":104,"pool":"par-system"}]
EOF
	[ "$(vms_with_tag par-system par-controller)" = 102 ]
	[ "$(vms_with_tag par-system par-managed | tr '\n' ' ')" = "101 102 " ]
	[ -z "$(vms_with_tag par-runners par-managed)" ]
}

@test "managed_vms lists clones before templates and skips VMs that aren't ours" {
	resources <<'EOF'
[{"vmid":10099,"pool":"par-runners","template":1,"tags":"par-managed;par-template"},
 {"vmid":10000,"pool":"par-runners","tags":"par-managed;par-worker"},
 {"vmid":10001,"pool":"par-runners"},
 {"vmid":101,"pool":"par-system","tags":"par-managed;par-gateway"},
 {"vmid":200,"pool":"elsewhere","tags":"par-managed"}]
EOF
	[ "$(managed_vms | tr '\n' ' ')" = "101 10000 10099 " ]
}

@test "free_reserved_vmid picks the first free ID at the end of the range" {
	resources <<'EOF'
[{"vmid":10096,"pool":"par-runners"},{"vmid":10097,"pool":"par-runners"}]
EOF
	PAR_VMID_START=10000
	PAR_VMID_END=10099
	[ "$(free_reserved_vmid)" = 10098 ]
}

@test "guest_exec passes on output and the exit status" {
	stub qm "echo '{\"exited\":1,\"exitcode\":3,\"out-data\":\"hello\\n\",\"err-data\":\"oops\\n\"}'"
	run guest_exec 105 30 -- false
	[ "$status" -eq 3 ]
	[[ $output == *hello* ]]
	[[ $output == *oops* ]]
	grep -qx 'qm guest exec 105 --timeout 30 -- false' "$CALLS"
}

@test "guest_exec reports a command that didn't finish in time" {
	stub qm "echo '{\"pid\":42}'"
	run guest_exec 105 5 -- sleep 60
	[ "$status" -eq 124 ]
}

@test "secrets reach the controller only on stdin" {
	stub qm 'cat >"$BATS_TEST_TMPDIR/stdin"; echo "{\"exited\":1,\"exitcode\":0}"'
	export BATS_TEST_TMPDIR
	printf 'the-secret\n' | write_controller_file 105 /etc/proxmox-actions-runners/pve-token
	[ "$(cat "$BATS_TEST_TMPDIR/stdin")" = the-secret ]
	run ! grep -q the-secret "$CALLS"
	grep -q -- '--pass-stdin 1 -- runuser -u parcon -- sh -c' "$CALLS"
}

@test "check records failures and warnings" {
	PREFLIGHT_FAILED=0
	WARNINGS=()
	check hard "passes" true
	check warn "warns" sh -c 'echo slow; exit 1'
	[ "$PREFLIGHT_FAILED" = 0 ]
	[ "${WARNINGS[0]}" = "warns: slow" ]
	run check hard "fails" sh -c 'echo broken; exit 1'
	[[ $output == "FAIL  fails: broken" ]]
}

@test "the worker subnet must not overlap the host's networks" {
	stub ip "case \"\$*\" in
		'-4 -o addr show') echo '2: vmbr0    inet 10.251.1.5/24 brd 10.251.1.255 scope global vmbr0' ;;
		'-4 route show') echo '10.251.1.0/24 dev vmbr0 proto kernel' ;;
	esac"
	settings_ok
	run check_worker_subnet
	[ "$status" -eq 1 ]
	[[ $output == *"overlaps 10.251.1.5/24"* ]]
	PAR_WORKER_SUBNET=10.252.0.0/22
	run check_worker_subnet
	[ "$status" -eq 0 ]
}

@test "the worker subnet must not overlap a static LAN address" {
	stub ip "true"
	settings_ok
	PAR_WORKER_SUBNET=192.0.2.0/24
	PAR_GATEWAY_IP=192.0.2.10/24
	run check_worker_subnet
	[ "$status" -eq 1 ]
	[[ $output == *"overlaps the LAN"* ]]
}

@test "names already in use by someone else stop the install" {
	stub pveum "case \"\$*\" in
		'pool list --output-format json') echo '[{\"poolid\":\"par-runners\"}]' ;;
		'role list --output-format json') echo '[]' ;;
		'user list --output-format json') echo '[]' ;;
	esac"
	stub pvesh "exit 1"
	run check_names
	[ "$status" -eq 1 ]
	[[ $output == *"pool par-runners"* ]]
}

@test "an earlier run's objects are ours" {
	stub pveum "echo '[{\"poolid\":\"par-system\",\"comment\":\"proxmox-actions-runners\"}]'"
	run check_names
	[ "$status" -eq 0 ]
	[[ $output == *"continuing it"* ]]
}

@test "VMIDs in the range must be free or in the runner pool" {
	resources <<'EOF'
[{"vmid":10005,"pool":"par-runners"},{"vmid":10050},{"vmid":999}]
EOF
	settings_ok
	run check_vmids
	[ "$status" -eq 1 ]
	[[ $output == *"VMIDs 10050"* ]]
}

@test "gateway_block covers the LAN, the API address, and the controller" {
	stub ip "echo '2: vmbr0    inet 192.0.2.5/24 brd 192.0.2.255 scope global vmbr0'"
	settings_ok
	CONTROLLER_ADDRESS=192.0.2.77
	[ "$(gateway_block)" = "192.0.2.0/24 192.0.2.5 192.0.2.77" ]
}

@test "render_config writes the App only once there is one" {
	stub ip "echo '2: vmbr0    inet 192.0.2.5/24 brd 192.0.2.255 scope global vmbr0'"
	stub openssl "echo 'sha256 Fingerprint=AA:BB'"
	stub pvesh "case \"\$*\" in
		'get /nodes --output-format json') echo '[{\"node\":\"pve1\"}]' ;;
		*status*) echo '{\"type\":\"lvmthin\"}' ;;
	esac"
	settings_ok
	PAR_LABELS=a,b
	run render_config
	[[ $output == *"url: https://192.0.2.5:8006/api2/json"* ]]
	[[ $output == *'tlsFingerprint: "AA:BB"'* ]]
	[[ $output == *"node: pve1"* ]]
	[[ $output == *"linkedClone: true"* ]]
	[[ $output == *"labels: [a, b]"* ]]
	[[ $output != *"app:"* ]]

	run render_config Iv23liEXAMPLE0000000
	[[ $output == *"clientId: Iv23liEXAMPLE0000000"* ]]
	[[ $output != *installationId* ]]
	[[ $output == *"privateKeyFile: /etc/proxmox-actions-runners/github-app.pem"* ]]

	run render_config Iv23liEXAMPLE0000000 7890123
	[[ $output == *"installationId: 7890123"* ]]
}

@test "app_query tells organizations and personal accounts apart" {
	PAR_GITHUB_URL=https://github.com/my-org
	[ "$(app_query)" = "org=my-org" ]
	stub curl "echo '{\"type\":\"Organization\"}'"
	PAR_GITHUB_URL=https://github.com/my-org/my-repo
	[ "$(app_query)" = "org=my-org&repo=my-repo" ]
	stub curl "echo '{\"type\":\"User\"}'"
	PAR_GITHUB_URL=https://github.com/octocat/hello
	[ "$(app_query)" = "user=octocat&repo=hello" ]
}

@test "download checks each image against the embedded checksum" {
	stub curl 'while (($#)); do [[ $1 == -o ]] && { echo image >"$2"; }; shift; done'
	WORK_DIR=$BATS_TEST_TMPDIR
	PAR_VERSION=0.1.0
	PAR_SHA256SUMS="$(printf image | sha256sum | cut -d' ' -f1)  par-gateway-0.1.0.qcow2"
	run download par-gateway-0.1.0.qcow2
	[ "$status" -eq 1 ]
	[[ $output == *"doesn't match its checksum"* ]]

	rm -f "$WORK_DIR/par-gateway-0.1.0.qcow2"
	PAR_SHA256SUMS="$(echo image | sha256sum | cut -d' ' -f1)  par-gateway-0.1.0.qcow2"
	run download par-gateway-0.1.0.qcow2
	[ "$status" -eq 0 ]
	grep -q "releases/download/v0.1.0/par-gateway-0.1.0.qcow2" "$CALLS"

	run download parcon-0.1.0.qcow2
	[ "$status" -eq 1 ]
	[[ $output == *"no checksum for parcon-0.1.0.qcow2"* ]]
}

@test "uninstall --dry-run shows the plan and changes nothing" {
	resources <<'EOF'
[{"vmid":101,"pool":"par-system","tags":"par-managed;par-controller"},
 {"vmid":10099,"pool":"par-runners","template":1,"tags":"par-managed;par-template"}]
EOF
	stub pveum "echo '[{\"poolid\":\"par-system\",\"comment\":\"proxmox-actions-runners\"}]'"
	stub qm "exit 1"
	DRY_RUN=1
	check_root() { true; }
	run do_uninstall
	[ "$status" -eq 0 ]
	[[ $output == *"destroy VMs: 101 10099"* ]]
	run ! grep -Eq '^(qm (stop|destroy)|pveum (pool|user|role) (delete|remove)|pvesh (delete|set))' "$CALLS"
}
