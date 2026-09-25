#!/usr/bin/env bash
# Runs on the test node as root, sent by run.sh. Imports the runner image (images/runner) as a template the way the
# installer will: in its own pool, on the worker VNet, with a cloud-init drive and the guest agent enabled, tagged
# with the image's runner version and build time. An existing template with the same tags is kept.
#
# Environment: VMID POOL STORAGE VNET IMAGE (the qcow2 on the node) TAGS (semicolon-separated)
set -euo pipefail

in_pool() { # in_pool <vmid>: whether the VM is a member of $POOL
	pvesh get "/pools/$POOL" --output-format json | perl -MJSON -e '
		my $pool = decode_json(do { local $/; <STDIN> });
		exit !grep { ($_->{vmid} // 0) == $ARGV[0] } @{$pool->{members}};' "$1"
}

main() {
	: "${VMID:?}" "${POOL:?}" "${STORAGE:?}" "${VNET:?}" "${IMAGE:?}" "${TAGS:?}"

	if qm status "$VMID" >/dev/null 2>&1; then
		in_pool "$VMID" || { echo "VM $VMID exists and isn't in pool $POOL; choose another VMID" >&2; exit 1; }
		if qm config "$VMID" | grep -qx "tags: $TAGS" && qm config "$VMID" | grep -q '^template: 1'; then
			echo "== runner image template $VMID is current"
			return
		fi
		echo "== replace runner image template $VMID"
		qm destroy "$VMID" --purge 1
	fi

	echo "== import $IMAGE as template $VMID"
	qm create "$VMID" --name par-it-runner-image --pool "$POOL" --memory 2048 --cores 2 --cpu host --ostype l26 \
		--scsihw virtio-scsi-single --net0 "virtio,bridge=$VNET" --agent enabled=1 --serial0 socket --vga serial0
	qm set "$VMID" --scsi0 "$STORAGE:0,import-from=$IMAGE" >/dev/null
	qm set "$VMID" --ide2 "$STORAGE:cloudinit" --boot order=scsi0 --ipconfig0 ip=dhcp --tags "$TAGS" >/dev/null
	qm template "$VMID"

	# /cluster/resources, which the controller reads, reports the template flag from pvestatd, about 10s late.
	local i
	for i in $(seq 1 60); do
		if pvesh get /cluster/resources --type vm --output-format json | perl -MJSON -e '
			my $id = $ARGV[0];
			exit !grep { $_->{vmid} == $id && $_->{template} } @{decode_json(do { local $/; <STDIN> })};' "$VMID"; then
			echo "reported as a template after ~${i}s"
			return
		fi
		sleep 1
	done
	echo "/cluster/resources still doesn't report VM $VMID as a template after 60s" >&2
	exit 1
}

main "$@"
