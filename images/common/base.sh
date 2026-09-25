#!/usr/bin/env bash
# First step of every image: an up-to-date system and the QEMU guest agent, which is how the controller reaches
# workers and the installer configures the gateway and controller VMs. The runner image turns automatic updates off
# (images/runner/scripts/base.sh); the long-lived gateway and controller turn them on (auto-updates.sh).
set -euo pipefail

main() {
  export DEBIAN_FRONTEND=noninteractive

  # Wait for cloud-init to finish its first boot before touching apt.
  cloud-init status --wait >/dev/null || true

  apt-get update
  apt-get -y -o Dpkg::Options::=--force-confold full-upgrade
  apt-get -y install --no-install-recommends \
    ca-certificates \
    qemu-guest-agent

  # Ubuntu starts the agent through a udev rule when the virtio serial port is present; make sure it also starts on
  # boot.
  systemctl enable qemu-guest-agent
}

main "$@"
