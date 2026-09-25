#!/usr/bin/env bash
# boot-test.sh IMAGE [PATTERN...] boots a finished image the way Proxmox first boots a VM made from it, on a throwaway
# overlay with a NoCloud seed and a guest agent channel, and checks its serial console. It catches what only the
# finished image shows, because cleanup.sh, the build's last step, installs units that first run on this boot. Every
# image must boot without failed units or ordering cycles, remove its build user, finish cloud-init, and start the
# guest agent; each PATTERN is a further console line (a grep regex) the image must show. No SSH is needed. Needs
# qemu-system-x86_64, qemu-img, and xorriso (all in the dev container) and KVM.
#
# The VM gets what Proxmox gives it: its first NIC at net0's PCI slot, with a known MAC, and a cloud-init network
# config like the one Proxmox writes for ipconfig0=ip=dhcp, which names the NIC eth0 by that MAC. NICS=2 adds a
# second NIC at net1's slot, as the gateway has. Cloud-init must finish within FINISH_SECONDS: an image whose NIC
# names don't match the config waits two minutes for the network before it gives up.
#
#   images/common/boot-test.sh output-runner/par-runner-dev.qcow2 'Started.*par-runner.path'
#   NICS=2 images/common/boot-test.sh output-gateway/par-gateway-dev.qcow2
set -euo pipefail

BOOT_SECONDS=${BOOT_SECONDS:-150}
FINISH_SECONDS=${FINISH_SECONDS:-90}
NICS=${NICS:-1}
TMP_DIR=""
# Proxmox's i440fx machine puts net0 and net1 at these PCI slots.
readonly NET0_SLOT=0x12 NET1_SLOT=0x13 NET0_MAC=bc:24:11:00:00:01

main() {
  local image=${1:?usage: boot-test.sh IMAGE [PATTERN...]}
  shift
  TMP_DIR=$(mktemp -d)
  trap 'rm -rf "$TMP_DIR"' EXIT

  mkdir "$TMP_DIR/seed"
  printf 'instance-id: par-boot-test\nlocal-hostname: par-boot-test\n' >"$TMP_DIR/seed/meta-data"
  printf '#cloud-config\n{}\n' >"$TMP_DIR/seed/user-data"
  cat >"$TMP_DIR/seed/network-config" <<EOF
version: 1
config:
    - type: physical
      name: eth0
      mac_address: '$NET0_MAC'
      subnets:
      - type: dhcp4
EOF
  xorriso -as genisoimage -output "$TMP_DIR/seed.iso" -volid cidata -joliet -rock "$TMP_DIR/seed" >/dev/null 2>&1
  qemu-img create -q -f qcow2 -F qcow2 -b "$(realpath "$image")" "$TMP_DIR/overlay.qcow2"

  local console=$TMP_DIR/console.log nics=(-netdev "user,id=n0" -device "virtio-net-pci,netdev=n0,addr=$NET0_SLOT,mac=$NET0_MAC")
  if ((NICS == 2)); then
    # The worker network: a NIC with a carrier and nothing on the other end.
    nics+=(-netdev "socket,id=n1,udp=127.0.0.1:1,localaddr=127.0.0.1:0" -device "virtio-net-pci,netdev=n1,addr=$NET1_SLOT")
  fi
  timeout "$BOOT_SECONDS" qemu-system-x86_64 -machine pc,accel=kvm -cpu host -smp 2 -m 2048 \
    -drive "file=$TMP_DIR/overlay.qcow2,if=none,id=d0,format=qcow2" -device virtio-scsi-pci -device scsi-hd,drive=d0 \
    -cdrom "$TMP_DIR/seed.iso" "${nics[@]}" \
    -chardev "socket,id=qga0,path=$TMP_DIR/qga.sock,server=on,wait=off" -device virtio-serial \
    -device virtserialport,chardev=qga0,name=org.qemu.guest_agent.0 \
    -display none -serial "file:$console" || true

  local failures=0
  expect() {
    if grep -aq "$2" "$console"; then echo "ok    $1"; else echo "FAIL  $1"; failures=$((failures + 1)); fi
  }
  refuse() {
    if grep -aq "$2" "$console"; then echo "FAIL  $1"; failures=$((failures + 1)); else echo "ok    $1"; fi
  }
  refuse "no systemd ordering cycles" "Ordering cycle"
  refuse "no failed units" "\[FAILED\]"
  expect "the image build user is removed on first boot" "Finished.*par-remove-build-user.service"
  expect "the guest agent started" "Started.*qemu-guest-agent.service"
  expect "cloud-init finished" "Cloud-init v\..*finished"
  refuse "the network came up without waiting for a timeout" "Timeout occurred while waiting for network"
  local up
  up=$(grep -ao 'Cloud-init v\..*finished.*Up [0-9.]* seconds' "$console" | grep -o 'Up [0-9.]*' | cut -d' ' -f2)
  if [[ -n $up ]] && ((${up%.*} < FINISH_SECONDS)); then
    echo "ok    cloud-init finished ${up%.*}s after boot"
  else
    echo "FAIL  cloud-init didn't finish within ${FINISH_SECONDS}s of boot (${up:-never})"
    failures=$((failures + 1))
  fi
  expect "the VM reached a login prompt" "login:"
  local pattern
  for pattern in "$@"; do
    expect "the console shows $pattern" "$pattern"
  done

  if ((failures > 0)); then
    echo "$failures check(s) failed; serial console:"
    tail -n 60 "$console"
    exit 1
  fi
}

main "$@"
