#!/usr/bin/env bash
# First step of the runner image: the QEMU guest agent, the runner user, and settings every worker needs. The
# runner, its one-job units, and the toolset follow in the numbered scripts.
set -euo pipefail

main() {
  export DEBIAN_FRONTEND=noninteractive

  # Wait for cloud-init to finish its first boot before touching apt.
  cloud-init status --wait >/dev/null || true

  apt-get update
  apt-get -y -o Dpkg::Options::=--force-confold full-upgrade
  apt-get -y install --no-install-recommends \
    ca-certificates \
    cloud-guest-utils \
    curl \
    qemu-guest-agent \
    sudo

  # The controller talks to workers only through the guest agent. Ubuntu starts it through a udev rule when the
  # virtio serial port is present; make sure it also starts on boot.
  systemctl enable qemu-guest-agent

  create_runner_user
  disable_background_updates
}

# The runner user matches GitHub-hosted runners: UID 1001 with passwordless sudo.
create_runner_user() {
  if ! id runner >/dev/null 2>&1; then
    useradd --uid 1001 --create-home --shell /bin/bash --groups adm,systemd-journal runner
  fi
  cat >/etc/sudoers.d/runner <<'EOF'
runner ALL=(ALL) NOPASSWD:ALL
Defaults:runner env_keep += "DEBIAN_FRONTEND"
EOF
  chmod 0440 /etc/sudoers.d/runner
  visudo -cf /etc/sudoers.d/runner
}

# Workers live for one job, and templates are rebuilt to pick up updates, so background apt activity only slows
# jobs down and makes workers differ from their template.
disable_background_updates() {
  systemctl disable --now apt-daily.timer apt-daily-upgrade.timer unattended-upgrades.service 2>/dev/null || true
  cat >/etc/apt/apt.conf.d/20auto-upgrades <<'EOF'
APT::Periodic::Update-Package-Lists "0";
APT::Periodic::Unattended-Upgrade "0";
EOF
}

main "$@"
