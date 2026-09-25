#!/usr/bin/env bash
# Automatic security updates for the gateway and controller images, after base.sh. These VMs are long-lived, unlike
# workers, so they keep themselves patched with unattended-upgrades.
set -euo pipefail

main() {
  export DEBIAN_FRONTEND=noninteractive
  apt-get -y install --no-install-recommends unattended-upgrades
  systemctl enable unattended-upgrades
  cat >/etc/apt/apt.conf.d/20auto-upgrades <<'EOF'
APT::Periodic::Update-Package-Lists "1";
APT::Periodic::Unattended-Upgrade "1";
EOF
}

main "$@"
