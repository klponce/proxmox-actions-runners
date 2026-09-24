#!/usr/bin/env bash
# Runs on the test node as root, sent by run.sh. Builds the integration suite's test template: the Ubuntu 26.04 cloud
# image with the QEMU guest agent, /run/par-runner created at boot, and the tags the controller looks for.
#
# This is a test fixture, not how the product builds templates: it installs the agent with a cloud-init snippet,
# which needs the snippets content type on the local storage. run.sh's teardown restores that setting.
#
# Environment: VMID POOL STORAGE BRIDGE (for the build VM's internet access) VNET (for the finished template)
#              REBUILD (1 to replace an existing template)
set -euo pipefail

readonly IMG=ubuntu-26.04-server-cloudimg-amd64.img
readonly IMG_URL=https://cloud-images.ubuntu.com/releases/26.04/release/$IMG
readonly IMG_SHA256=4908fb59ccd4e87ae4e8e973b7ef56f535448eacb24a87fd787270c0048987bc
readonly IMPORT_DIR=/var/lib/vz/import
readonly SNIPPET=par-it-agent.yaml
# Holds the local storage's content types from before the suite added snippets, for teardown.
readonly CONTENT_MARKER=/root/.par-it-local-content

in_pool() { # in_pool <vmid>: whether the VM is a member of $POOL
	# JSON on stdin, the VMID as an argument (perl -n would treat arguments as input files).
	pvesh get "/pools/$POOL" --output-format json | perl -MJSON -e '
		my $pool = decode_json(do { local $/; <STDIN> });
		exit !grep { ($_->{vmid} // 0) == $ARGV[0] } @{$pool->{members}};' "$1"
}

enable_snippets() {
	local content
	content=$(pvesh get /storage/local --output-format json | perl -MJSON -0777 -ne 'print decode_json($_)->{content}')
	[[ -e $CONTENT_MARKER ]] || printf '%s\n' "$content" >"$CONTENT_MARKER"
	[[ ,$content, == *,snippets,* ]] || pvesm set local --content "$content,snippets"
	mkdir -p /var/lib/vz/snippets
	cat >/var/lib/vz/snippets/$SNIPPET <<'EOF'
#cloud-config
package_update: true
packages: [qemu-guest-agent]
write_files:
  # The controller writes the JIT config to /run/par-runner/jitconfig; the guest agent can't create directories.
  - path: /etc/tmpfiles.d/par-runner.conf
    content: "d /run/par-runner 0700 root root -\n"
runcmd:
  - [systemctl, enable, --now, qemu-guest-agent]
EOF
}

download_image() {
	mkdir -p "$IMPORT_DIR"
	if echo "$IMG_SHA256  $IMPORT_DIR/$IMG" | sha256sum -c --status 2>/dev/null; then
		return
	fi
	echo "== download $IMG"
	curl -fsSL -o "$IMPORT_DIR/$IMG.part" "$IMG_URL"
	echo "$IMG_SHA256  $IMPORT_DIR/$IMG.part" | sha256sum -c --quiet -
	mv "$IMPORT_DIR/$IMG.part" "$IMPORT_DIR/$IMG"
}

main() {
	: "${VMID:?}" "${POOL:?}" "${STORAGE:?}" "${BRIDGE:?}" "${VNET:?}"

	if qm status "$VMID" >/dev/null 2>&1; then
		in_pool "$VMID" || { echo "VM $VMID exists and isn't in pool $POOL; choose another PAR_IT_TEMPLATE_VMID" >&2; exit 1; }
		if [[ ${REBUILD:-0} != 1 ]] && qm config "$VMID" | grep -q '^template: 1'; then
			echo "== template $VMID exists; set PAR_IT_REBUILD_TEMPLATE=1 to rebuild it"
			return
		fi
		echo "== destroy old VM $VMID"
		qm stop "$VMID" >/dev/null 2>&1 || true
		qm destroy "$VMID" --purge 1
	fi

	download_image
	enable_snippets

	echo "== build VM $VMID"
	qm create "$VMID" --name par-it-template --pool "$POOL" --memory 2048 --cores 2 --cpu host --ostype l26 \
		--scsihw virtio-scsi-single --net0 "virtio,bridge=$BRIDGE" --agent enabled=1 --serial0 socket --vga serial0
	qm set "$VMID" --scsi0 "$STORAGE:0,import-from=$IMPORT_DIR/$IMG" >/dev/null
	qm set "$VMID" --ide2 "$STORAGE:cloudinit" --boot order=scsi0 --ipconfig0 ip=dhcp \
		--cicustom "user=local:snippets/$SNIPPET" >/dev/null
	qm resize "$VMID" scsi0 8G >/dev/null
	qm start "$VMID"

	echo "== wait for the guest agent (cloud-init installs it; needs DHCP and internet on $BRIDGE)"
	local up=0
	for i in $(seq 1 120); do
		if qm agent "$VMID" ping >/dev/null 2>&1; then
			echo "agent up after ~$((i * 5))s"
			up=1
			break
		fi
		sleep 5
	done
	[[ $up == 1 ]] || { echo "the guest agent didn't come up within 10 minutes" >&2; exit 1; }

	echo "== reset identity and cloud-init state"
	qm guest exec "$VMID" --timeout 300 -- cloud-init status --wait >/dev/null || true
	qm guest exec "$VMID" --timeout 60 -- sh -c \
		'cloud-init clean --logs --seed --machine-id && rm -f /etc/ssh/ssh_host_* && sync' >/dev/null

	echo "== convert to a template on $VNET"
	qm shutdown "$VMID" --timeout 180
	qm set "$VMID" --delete cicustom --net0 "virtio,bridge=$VNET" --tags "par-managed;par-template;par-tv-1" >/dev/null
	qm template "$VMID"

	# /cluster/resources, which the controller reads, reports the template flag from pvestatd, about 10s late.
	echo "== wait until /cluster/resources reports it as a template"
	local i
	for i in $(seq 1 60); do
		if pvesh get /cluster/resources --type vm --output-format json | perl -MJSON -e '
			my $id = $ARGV[0];
			exit !grep { $_->{vmid} == $id && $_->{template} } @{decode_json(do { local $/; <STDIN> })};' "$VMID"; then
			echo "reported after ~${i}s"
			return
		fi
		sleep 1
	done
	echo "/cluster/resources still doesn't report VM $VMID as a template after 60s" >&2
	exit 1
}

main "$@"
