#!/usr/bin/env bash
# boot-test.sh IMAGE boots a finished runner image the way a worker first boots, on a throwaway overlay with a
# NoCloud seed, and checks its serial console. It catches what only the finished image shows, because cleanup.sh,
# the build's last step, installs units that first run on this boot. No SSH is needed. Needs qemu-system-x86_64,
# qemu-img, and xorriso (all in the dev container) and KVM.
set -euo pipefail

BOOT_SECONDS=${BOOT_SECONDS:-120}
TMP_DIR=""

main() {
  local image=${1:?usage: boot-test.sh IMAGE}
  TMP_DIR=$(mktemp -d)
  trap 'rm -rf "$TMP_DIR"' EXIT

  mkdir "$TMP_DIR/seed"
  printf 'instance-id: par-boot-test\nlocal-hostname: par-boot-test\n' >"$TMP_DIR/seed/meta-data"
  printf '#cloud-config\n{}\n' >"$TMP_DIR/seed/user-data"
  xorriso -as genisoimage -output "$TMP_DIR/seed.iso" -volid cidata -joliet -rock "$TMP_DIR/seed" >/dev/null 2>&1
  qemu-img create -q -f qcow2 -F qcow2 -b "$(realpath "$image")" "$TMP_DIR/overlay.qcow2"

  local console=$TMP_DIR/console.log
  timeout "$BOOT_SECONDS" qemu-system-x86_64 -machine accel=kvm -cpu host -smp 2 -m 2048 \
    -drive "file=$TMP_DIR/overlay.qcow2,if=none,id=d0,format=qcow2" -device virtio-scsi-pci -device scsi-hd,drive=d0 \
    -cdrom "$TMP_DIR/seed.iso" -netdev user,id=n0 -device virtio-net-pci,netdev=n0 \
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
  expect "par-runner.path waits for a JIT config" "Started.*par-runner.path"
  expect "cloud-init finished" "Cloud-init v\..*finished"
  expect "the VM reached a login prompt" "login:"

  if ((failures > 0)); then
    echo "$failures check(s) failed; serial console:"
    tail -n 60 "$console"
    exit 1
  fi
}

main "$@"
