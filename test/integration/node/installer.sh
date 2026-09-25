#!/usr/bin/env bash
# Runs on the test node as root, sent by run.sh. Drives install.sh's own install steps without GitHub: the Proxmox
# access objects, the worker network, the runner template, the gateway and controller VMs, and the controller's
# configuration, then the worker network checks of install.sh's smoke test.
#
# It sources install.sh as its bats tests do and uses images from DIR instead of a GitHub release: download() keeps a
# file that is already in WORK_DIR and still checks it against the checksums, which are set here from those files. It
# stops before the GitHub App (setup_app) and doesn't start the controller, whose scale set session needs the App.
#
# Environment: DIR (holds install.sh, answers, and the images) VERSION (the images' version)
#
# shellcheck disable=SC2034 # PAR_VERSION, PAR_SHA256SUMS, ASSUME_YES, and WORK_DIR are install.sh's, read by its steps.
set -euo pipefail

: "${DIR:?}" "${VERSION:?}"
cd "$DIR"

# shellcheck source=/dev/null
PAR_INSTALL_SOURCED=1 . ./install.sh

assets=("par-runner-$VERSION.qcow2" "par-runner-$VERSION.json" "par-gateway-$VERSION.qcow2" "parcon-$VERSION.qcow2")
PAR_VERSION=$VERSION
PAR_SHA256SUMS=$(sha256sum "${assets[@]}")
ASSUME_YES=1
WORK_DIR=$DIR

load_answers answers
apply_defaults
validate_settings
preflight

step "Verify images"
for asset in "${assets[@]}"; do
	download "$asset"
done

create_access
create_network
import_template
create_gateway
create_controller
configure_controller

# The first half of smoke_test; the second half waits for the controller's scale set session, which needs the App.
step "Worker network"
template=$(release_template)
vmid=$(free_reserved_vmid) || die "no free reserved VMID for the network check"
change qm clone "$template" "$vmid" --name par-it-network-check --pool "$RUNNER_POOL"
change qm set "$vmid" --tags "$TAG_MANAGED;$TAG_BUILD" --ciupgrade 0
change qm start "$vmid"
wait_agent "$vmid"
failures=()
address=$(vm_ipv4 "$vmid") || failures+=("the worker got no DHCP lease")
guest_exec "$vmid" 30 -- curl -sS -o /dev/null --max-time 15 https://api.github.com/ ||
	failures+=("the worker can't reach the internet")
! guest_exec "$vmid" 30 -- curl -sk -o /dev/null --max-time 5 "https://$(pve_address):8006/" 2>/dev/null ||
	failures+=("the worker can reach the Proxmox API")
! guest_exec "$vmid" 30 -- timeout 5 bash -c "</dev/tcp/$CONTROLLER_ADDRESS/22" 2>/dev/null ||
	failures+=("the worker can reach the controller VM")
change qm stop "$vmid" --skiplock 1
change qm destroy "$vmid" --purge 1
((${#failures[@]} == 0)) || die "worker network: ${failures[*]}"
say "    worker $vmid got $address from the gateway, reaches the internet, and can't reach the Proxmox API" \
	"or the controller"

# The controller's own check, from inside its VM, with the config and token the installer wrote.
say "    parcon check proxmox in controller VM $CONTROLLER_VMID:"
as_parcon "$CONTROLLER_VMID" 60 parcon check proxmox | sed 's/^/      /'
echo "PAR_IT_CONTROLLER_VMID=$CONTROLLER_VMID"
