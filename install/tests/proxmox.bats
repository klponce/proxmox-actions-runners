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

@test "system_vmid keeps the gateway and controller out of the worker range" {
	PAR_VMID_START=100
	PAR_VMID_END=199
	echo '[{"vmid":150},{"vmid":200},{"vmid":201}]' >"$BATS_TEST_TMPDIR/resources.json"
	stub pvesh "case \"\$*\" in
		'get /cluster/nextid') echo 101 ;;
		'get /cluster/resources'*) cat '$BATS_TEST_TMPDIR/resources.json' ;;
	esac"
	[ "$(system_vmid)" = 202 ]

	PAR_VMID_START=10000
	PAR_VMID_END=10099
	[ "$(system_vmid)" = 101 ]
}

@test "replace_gateway keeps the old gateway's NICs and address, with the gateway's hardware" {
	stub qm "case \"\$*\" in
		'config 105') printf '%s\n' 'net0: virtio=BC:24:11:00:00:01,bridge=vmbr0' \
			'net1: virtio=BC:24:11:00:00:02,bridge=parnet' 'ipconfig0: ip=192.0.2.10/24,gw=192.0.2.1' ;;
		*is-system-running*) echo '{\"exited\":1,\"exitcode\":0,\"out-data\":\"running\\n\"}' ;;
		'guest exec 105 '*) echo '{\"exited\":1,\"exitcode\":0,\"out-data\":\"WORKER_SUBNET=10.251.0.0/22\\n\"}' ;;
	esac"
	PAR_VERSION=0.1.0
	PAR_STORAGE=local-lvm
	WORK_DIR=/var/tmp/par-install.test
	GATEWAY_VMID=105
	run replace_gateway
	[ "$status" -eq 0 ]
	grep -qx 'qm create 105 --name par-gateway --pool par-system --memory 1024 --cores 1 --cpu host --ostype l26 --scsihw virtio-scsi-single --net0 virtio=BC:24:11:00:00:01,bridge=vmbr0 --agent enabled=1 --onboot 1 --serial0 socket --vga serial0 --tags par-managed;par-gateway;par-release-0.1.0 --net1 virtio=BC:24:11:00:00:02,bridge=parnet' "$CALLS"
	grep -qx 'qm set 105 --ide2 local-lvm:cloudinit --boot order=scsi0 --ipconfig0 ip=192.0.2.10/24,gw=192.0.2.1 --ciupgrade 0' "$CALLS"
	grep -qx 'qm start 105' "$CALLS"
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

@test "gateway_block covers the LAN, every host address, and the controller" {
	# The host has a second, public address on another interface; the API listens there too.
	stub ip "case \"\$*\" in
		*'dev vmbr0') echo '2: vmbr0    inet 192.0.2.5/24 brd 192.0.2.255 scope global vmbr0' ;;
		*) printf '%s\n' '1: lo    inet 127.0.0.1/8 scope host lo' \
			'2: vmbr0    inet 192.0.2.5/24 brd 192.0.2.255 scope global vmbr0' \
			'3: vmbr1    inet 203.0.113.9/24 brd 203.0.113.255 scope global vmbr1' ;;
	esac"
	settings_ok
	CONTROLLER_ADDRESS=192.0.2.77
	[ "$(gateway_block)" = "192.0.2.0/24 192.0.2.5 192.0.2.77 203.0.113.9" ]
}

@test "render_config writes the App only once there is one" {
	stub ip "echo '2: vmbr0    inet 192.0.2.5/24 brd 192.0.2.255 scope global vmbr0'"
	stub openssl "exit 1"
	stub pvesh "case \"\$*\" in
		'get /nodes --output-format json') echo '[{\"node\":\"pve1\"}]' ;;
		*status*) echo '{\"type\":\"lvmthin\"}' ;;
	esac"
	settings_ok
	PAR_LABELS=a,b
	run render_config
	[[ $output == *"url: https://192.0.2.5:8006/api2/json"* ]]
	# No certificates in this test: the node's own certificate is assumed, verified against the node's CA.
	[[ $output == *"caCertFile: /etc/proxmox-actions-runners/pve-ca.pem"* ]]
	[[ $output != *tlsFingerprint* ]]
	[[ $output == *"node: pve1"* ]]
	[[ $output == *"linkedClone: true"* ]]
	[[ $output == *'labels: ["a", "b"]'* ]]
	[[ $output == *'name: "proxmox-ubuntu-26.04"'* ]]
	[[ $output != *"github:"* ]]

	run render_config Iv23liEXAMPLE0000000
	[[ $output == *"clientId: Iv23liEXAMPLE0000000"* ]]
	[[ $output != *installationId* ]]
	[[ $output != *configUrl* ]]
	[[ $output == *"privateKeyFile: /etc/proxmox-actions-runners/github-app.pem"* ]]

	PAR_GITHUB_URL=https://github.com/my-org
	run render_config Iv23liEXAMPLE0000000 7890123
	[[ $output == *"configUrl: https://github.com/my-org"* ]]
	[[ $output == *"installationId: 7890123"* ]]
}

@test "render_config keeps labels that look like YAML values strings" {
	stub ip "echo '2: vmbr0    inet 192.0.2.5/24 brd 192.0.2.255 scope global vmbr0'"
	stub openssl "exit 1"
	stub pvesh "case \"\$*\" in
		'get /nodes --output-format json') echo '[{\"node\":\"pve1\"}]' ;;
		*status*) echo '{\"type\":\"lvmthin\"}' ;;
	esac"
	settings_ok
	PAR_LABELS=null,true,1.5
	validate_settings
	run render_config
	[[ $output == *'labels: ["null", "true", "1.5"]'* ]]
}

@test "downloads need room where they go, not on the VM storage" {
	stub df "printf '%s\n' 'Filesystem 1024-blocks Used Available Capacity Mounted on' \
		'/dev/mapper/pve-root 98559220 90000000 3145728 97% /'" # 3 GiB free
	run check_download_space
	[ "$status" -eq 1 ]
	[[ $output == "3 GiB free, 6 GiB needed" ]]
	grep -qx 'df -Pk /var/tmp' "$CALLS"

	stub df "printf '%s\n' 'Filesystem 1024-blocks Used Available Capacity Mounted on' \
		'/dev/mapper/pve-root 98559220 50000000 41943040 55% /'" # 40 GiB free
	run check_download_space
	[ "$status" -eq 0 ]
}

@test "pending SDN changes other than ours stop the install" {
	stub pvesh "case \"\$*\" in
		'get /cluster/sdn/zones --pending 1'*) echo '[{\"zone\":\"parzone\",\"state\":\"new\"},{\"zone\":\"lab\"},{\"zone\":\"dmz\",\"state\":\"changed\"}]' ;;
		'get /cluster/sdn/vnets --pending 1'*) echo '[{\"vnet\":\"parnet\",\"state\":\"new\"},{\"vnet\":\"vlan20\",\"state\":\"deleted\"}]' ;;
	esac"
	[ "$(pending_sdn | tr '\n' ' ')" = "dmz vlan20 " ]
	run check_pending_sdn
	[ "$status" -eq 1 ]
	[[ $output == *"pending SDN changes to dmz vlan20 would be applied too"* ]]

	# Only our own zone and VNet pending, as on a re-run: fine.
	stub pvesh "case \"\$*\" in
		'get /cluster/sdn/zones --pending 1'*) echo '[{\"zone\":\"parzone\",\"state\":\"new\"}]' ;;
		*) echo '[]' ;;
	esac"
	run check_pending_sdn
	[ "$status" -eq 0 ]
}

@test "an organization's installation serves the organization" {
	run installation_target '{"installationId":1,"account":"my-org","accountType":"Organization"}'
	[ "$status" -eq 0 ]
	[ "$output" = https://github.com/my-org ]
}

@test "a personal account's installation serves its repository" {
	run installation_target '{"installationId":1,"account":"octocat","accountType":"User","repositories":["hello"]}'
	[ "$status" -eq 0 ]
	[ "$output" = https://github.com/octocat/hello ]

	run installation_target '{"installationId":1,"account":"octocat","accountType":"User"}'
	[ "$status" -eq 1 ]
	[[ $output == *"without a repository"* ]]
}

@test "with several repositories, PAR_GITHUB_URL picks one, and without a terminal nothing is guessed" {
	local several='{"installationId":1,"account":"octocat","accountType":"User","repositories":["hello","world"]}'
	PAR_GITHUB_URL=https://github.com/octocat/world
	run installation_target "$several"
	[ "$status" -eq 0 ]
	[ "$output" = https://github.com/octocat/world ]

	PAR_GITHUB_URL=""
	run installation_target "$several" </dev/null
	[ "$status" -eq 1 ]
	[[ $output == *"set PAR_GITHUB_URL"* && $output == *"hello world"* ]]
}

@test "the state is 32 random bytes in base64url" {
	local a b
	a=$(new_state)
	b=$(new_state)
	[[ $a =~ ^[A-Za-z0-9_-]{43}$ ]]
	[ "$a" != "$b" ]
}

@test "existing_github reads what an earlier run learned" {
	stub qm "echo '{\"exited\":1,\"exitcode\":0,\"out-data\":\"github:\\n  app:\\n    clientId: Iv23liEXAMPLE0000000\\n    privateKeyFile: /etc/k\\n\"}'"
	CONTROLLER_VMID=105
	[ "$(existing_github)" = "- Iv23liEXAMPLE0000000 - " ]
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
	[[ $output == *"now: 101 10099"* ]]
	run ! grep -Eq '^(qm (stop|destroy)|pveum (pool|user|role) (delete|remove)|pvesh (delete|set))' "$CALLS"
}

@test "uninstall lists the VMs again once the controller has stopped" {
	# At the plan, worker 10000 exists. While the controller stops, it destroys 10000 and creates 10001.
	resources <<'EOF'
[{"vmid":101,"pool":"par-system","tags":"par-managed;par-controller"},
 {"vmid":10000,"pool":"par-runners","tags":"par-managed;par-worker"},
 {"vmid":10099,"pool":"par-runners","template":1,"tags":"par-managed;par-template"}]
EOF
	stub pveum "echo '[{\"poolid\":\"par-system\",\"comment\":\"proxmox-actions-runners\"}]'"
	stub qm "case \"\$*\" in
		'status 101') echo 'status: running' ;;
		'status '*) echo 'status: stopped' ;;
		'guest exec 101 '*systemctl*)
			cat >'$BATS_TEST_TMPDIR/resources.json' <<'JSON'
[{\"vmid\":101,\"pool\":\"par-system\",\"tags\":\"par-managed;par-controller\"},
 {\"vmid\":10001,\"pool\":\"par-runners\",\"tags\":\"par-managed;par-worker\"},
 {\"vmid\":10099,\"pool\":\"par-runners\",\"template\":1,\"tags\":\"par-managed;par-template\"}]
JSON
			echo '{\"exited\":1,\"exitcode\":0}' ;;
		'guest exec '*) echo '{\"exited\":1,\"exitcode\":0}' ;;
	esac"
	ASSUME_YES=1
	check_root() { true; }
	run do_uninstall
	[ "$status" -eq 0 ]
	grep -qx 'qm destroy 10001 --purge 1' "$CALLS"
	run ! grep -qx 'qm destroy 10000 --purge 1' "$CALLS"
	grep -qx 'qm destroy 101 --purge 1' "$CALLS"
	grep -qx 'qm destroy 10099 --purge 1' "$CALLS"
	# It waits as long as the controller's stop can take.
	grep -qx "qm guest exec 101 --timeout $CONTROLLER_STOP_TIMEOUT -- systemctl disable --now parcon.service" "$CALLS"
}

@test "the installer waits longer than parcon.service may take to stop" {
	local stop
	stop=$(sed -n 's/^TimeoutStopSec=//p' "$BATS_TEST_DIRNAME/../../deploy/parcon.service")
	case $stop in
	*min) stop=$((${stop%min} * 60)) ;;
	*s) stop=${stop%s} ;;
	esac
	[[ $stop =~ ^[0-9]+$ ]]
	((CONTROLLER_STOP_TIMEOUT > stop))
}

@test "destroy_vm counts a VM that is already gone as destroyed" {
	stub qm "exit 2" # Proxmox: Configuration file '...' does not exist
	run destroy_vm 10000
	[ "$status" -eq 0 ]

	stub qm "case \"\$1\" in status) echo 'status: stopped' ;; *) exit 1 ;; esac"
	run destroy_vm 10000
	[ "$status" -eq 1 ]
	[[ $output == *"destroying VM 10000 failed"* ]]
}

@test "free space needs 50 GiB" {
	storage_json() { echo '{"avail":53687091200}'; } # 50 GiB
	run check_free_space
	[ "$status" -eq 0 ]
	[ "$output" = "50 GiB free, 50 GiB needed" ]
	storage_json() { echo '{"avail":53687091199}'; }
	run ! check_free_space
	[ "$output" = "49 GiB free, 50 GiB needed" ]
}

@test "wait_booted waits for the boot to finish and accepts a degraded one" {
	stub qm 'case "$*" in
		"guest cmd"*) ;;
		*) echo "{\"exited\":1,\"exitcode\":0,\"out-data\":\"running\\n\"}" ;;
	esac'
	run wait_booted 100
	[ "$status" -eq 0 ]
	grep -q '^qm guest exec 100 --timeout 600 -- systemctl is-system-running --wait$' "$CALLS"

	stub qm 'case "$*" in
		"guest cmd"*) ;;
		*) echo "{\"exited\":1,\"exitcode\":1,\"out-data\":\"degraded\\n\"}" ;;
	esac'
	run wait_booted 100
	[ "$status" -eq 0 ]
	[[ $output == *"finished booting with a failed unit"* ]]
}

@test "wait_booted stops the install when the boot doesn't finish" {
	stub qm 'case "$*" in
		"guest cmd"*) ;;
		*) echo "{\"pid\":42}" ;;
	esac'
	run wait_booted 100
	[ "$status" -eq 1 ]
	[[ $output == *"didn't finish booting within 10 minutes"* ]]
}

@test "change_quiet drops a command's output but keeps its errors and the log line" {
	# The command prints "progress 42", which its own text in the log line doesn't contain.
	run change_quiet sh -c 'echo progress $((40 + 2)); echo broken >&2; exit 3'
	[ "$status" -eq 3 ]
	[[ $output == *'$ sh -c'* ]]
	[[ $output == *broken* ]]
	[[ $output != *"progress 42"* ]]
}
