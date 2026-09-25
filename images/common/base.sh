#!/usr/bin/env bash
# First step of the gateway and controller images: an up-to-date system, the QEMU guest agent the installer drives
# them through, and automatic security updates. These VMs are long-lived, unlike workers, so they keep themselves
# patched with unattended-upgrades. The runner image has its own base.sh, which turns automatic updates off.
set -euo pipefail

main() {
  export DEBIAN_FRONTEND=noninteractive

  # Wait for cloud-init to finish its first boot before touching apt.
  cloud-init status --wait >/dev/null || true

  apt-get update
  apt-get -y -o Dpkg::Options::=--force-confold full-upgrade
  apt-get -y install --no-install-recommends \
    ca-certificates \
    qemu-guest-agent \
    unattended-upgrades

  systemctl enable qemu-guest-agent unattended-upgrades
  cat >/etc/apt/apt.conf.d/20auto-upgrades <<'EOF'
APT::Periodic::Update-Package-Lists "1";
APT::Periodic::Unattended-Upgrade "1";
EOF
}

main "$@"
