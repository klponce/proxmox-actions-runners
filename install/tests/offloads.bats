#!/usr/bin/env bats
# The LAN NIC's offloads on a node that is itself a VM, against a fake sysfs and a fake ethtool.

load helpers

# nic NAME [DRIVER]: a NIC on the LAN bridge vmbr0, with a device bound to DRIVER. Without a driver, it has no
# device, like a VM's tap.
nic() {
	mkdir -p "$SYS_NET/$1" "$SYS_NET/vmbr0/brif"
	touch "$SYS_NET/vmbr0/brif/$1"
	if [[ -n ${2:-} ]]; then
		mkdir -p "$SYS_NET/$1/device" "$BATS_TEST_TMPDIR/sys/bus/virtio/drivers/$2"
		ln -s "$BATS_TEST_TMPDIR/sys/bus/virtio/drivers/$2" "$SYS_NET/$1/device/driver"
	fi
}

# fake_ethtool stubs ethtool: -k shows the offloads as on until -K turns them off, and -K NIC gro on turns them on.
fake_ethtool() {
	cat >"$BATS_TEST_TMPDIR/ethtool" <<'SH'
#!/usr/bin/env bash
state="$(dirname "$0")/off-$2"
case $1 in
-k)
	s=on
	[[ ! -e $state ]] || s=off
	printf '%s\n' "rx-checksumming: on [fixed]" "tx-checksumming: $s" "tcp-segmentation-offload: $s" \
		"	tx-tcp-segmentation: $s" "generic-segmentation-offload: $s" "generic-receive-offload: $s" \
		"large-receive-offload: off [fixed]"
	;;
-K) if [[ $4 == off ]]; then touch "$state"; else rm -f "$state"; fi ;;
esac
SH
	chmod +x "$BATS_TEST_TMPDIR/ethtool"
	stub ethtool "'$BATS_TEST_TMPDIR/ethtool' \"\$@\""
}

@test "a node that isn't a VM gets no offload changes" {
	settings_ok
	fake_ethtool
	nic eno1 e1000e
	nic tap100i0
	run check_offloads
	[ "$status" -eq 0 ]
	[ "$output" = "not needed: no virtio NIC on vmbr0" ]
	[ -z "$(offload_plan)" ]
	run tune_offloads
	[ "$status" -eq 0 ]
	[ -z "$output" ]
	[ ! -e "$OFFLOAD_RULE" ]
	run ! grep -q '^ethtool' "$CALLS"
}

@test "on a node that is a VM, install turns the offloads off now and at each boot" {
	settings_ok
	fake_ethtool
	nic nic0 virtio_net
	nic tap100i0
	COMMAND=install
	run check_offloads
	[ "$status" -eq 0 ]
	[ "$output" = "the node is a VM: they will be turned off on nic0" ]
	[[ $(offload_plan) == *"turn off offloads (gro gso tso tx) on nic0, the virtio NIC under vmbr0"* ]]

	run tune_offloads
	[ "$status" -eq 0 ]
	grep -qx 'ethtool -K nic0 gro off gso off tso off tx off' "$CALLS"
	run ! grep -q 'tap100i0' "$CALLS"
	grep -qx "ACTION==\"add\", SUBSYSTEM==\"net\", NAME==\"nic0\", RUN+=\"$STUBS/ethtool -K nic0 gro off gso off tso off tx off\"" \
		"$OFFLOAD_RULE"
	[ "$(stat -c %a "$OFFLOAD_RULE")" = 644 ]

	# Done: check passes, the plan has nothing to add, and a second run changes nothing.
	COMMAND=check
	run check_offloads
	[ "$status" -eq 0 ]
	[ "$output" = "off on nic0" ]
	[ -z "$(offload_plan)" ]
	: >"$CALLS"
	run tune_offloads
	[ "$status" -eq 0 ]
	[[ $output != *'$ '* ]]
	run ! grep -q '^ethtool -K' "$CALLS"
}

@test "check warns until the offloads are off for good" {
	settings_ok
	fake_ethtool
	nic nic0 virtio_net
	COMMAND=check
	run ! check_offloads
	[[ $output == *"udev rule $OFFLOAD_RULE; nic0: tx-checksumming tcp-segmentation-offload generic-segmentation-offload generic-receive-offload on"* ]]

	# Off now, but not at the next boot.
	ethtool -K nic0 gro off gso off tso off tx off
	run ! check_offloads
	[[ $output == *"(udev rule $OFFLOAD_RULE)"* ]]
}

@test "the offloads stay on when ethtool is missing" {
	settings_ok
	nic nic0 virtio_net
	command() { [[ $* != "-v ethtool" ]] && builtin command "$@"; }
	COMMAND=check
	run ! check_offloads
	[ "$output" = "ethtool isn't installed, so they can't be turned off" ]
	[ -z "$(offload_plan)" ]
	run tune_offloads
	[ "$status" -eq 0 ]
	[[ $output == *"ethtool isn't installed"* ]]
	[ ! -e "$OFFLOAD_RULE" ]
}

@test "--dry-run changes no offloads" {
	settings_ok
	fake_ethtool
	nic nic0 virtio_net
	DRY_RUN=1
	run tune_offloads
	[ "$status" -eq 0 ]
	[[ $output == *'$ ethtool -K nic0 gro off gso off tso off tx off'* ]]
	[ ! -e "$OFFLOAD_RULE" ]
	run ! grep -q '^ethtool -K' "$CALLS"
}

@test "uninstall removes the rule and turns the offloads back on" {
	settings_ok
	fake_ethtool
	nic nic0 virtio_net
	tune_offloads >/dev/null
	[ "$(offload_rule_ports)" = nic0 ]
	: >"$CALLS"
	run remove_offload_rule
	[ "$status" -eq 0 ]
	grep -qx 'ethtool -K nic0 gro on gso on tso on tx on' "$CALLS"
	[ ! -e "$OFFLOAD_RULE" ]
	[ -n "$(offloads_on nic0)" ]
}

@test "an upgrade finds the LAN bridge on the gateway VM" {
	stub qm "echo 'net0: virtio=BC:24:11:00:00:01,bridge=vmbr1,firewall=1'"
	as_parcon() { printf '%s\n' 'proxmox:' '  storage: local-lvm' '  vmidRange: { start: 10000, end: 10099 }'; }
	GATEWAY_VMID=105
	load_installed_settings
	[ "$PAR_BRIDGE" = vmbr1 ]
}
