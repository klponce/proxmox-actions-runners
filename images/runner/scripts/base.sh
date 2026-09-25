#!/usr/bin/env bash
# The runner image's step after images/common/base.sh: the runner user and settings every worker needs. The
# runner, its one-job units, and the toolset follow in the numbered scripts.
set -euo pipefail

main() {
  export DEBIAN_FRONTEND=noninteractive
  apt-get -y install --no-install-recommends \
    cloud-guest-utils \
    curl \
    sudo

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
