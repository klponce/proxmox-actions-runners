#!/usr/bin/env bash
# The controller: the parcon binary, the parcon system user it runs as, its config directory, and its unit. The
# Packer build uploads the binary and deploy/parcon.service to /tmp. The unit stays disabled: the installer writes the
# config and secrets, creates the GitHub App, then enables it.
set -euo pipefail

CONFIG_DIR=/etc/proxmox-actions-runners

main() {
  if ! id parcon >/dev/null 2>&1; then
    useradd --system --user-group --no-create-home --home-dir /nonexistent --shell /usr/sbin/nologin parcon
  fi

  # Only parcon reads the config and secrets. The installer runs every parcon command as parcon
  # (runuser -u parcon -- parcon ...), so the files it writes get the right owner.
  install -d -o parcon -g parcon -m 0700 "$CONFIG_DIR"

  install -m 0755 /tmp/parcon /usr/local/bin/parcon
  install -m 0644 /tmp/parcon.service /etc/systemd/system/parcon.service
  systemctl daemon-reload
}

main "$@"
