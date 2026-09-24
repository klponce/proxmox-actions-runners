#!/usr/bin/env bash
# Installs the units that make a worker run exactly one job: wait for the JIT config the controller writes through
# the guest agent, run the runner with it, and power the VM off when the runner exits, which tells the controller
# the worker is done. The paths must match controller.JITConfigPath and controller.JITReadyPath.
set -euo pipefail

main() {
  # The guest agent writes as root; the directory must exist first, and only root may write to it.
  cat >/etc/tmpfiles.d/par-runner.conf <<'EOF'
d /run/par-runner 0700 root root -
EOF
  chmod 0644 /etc/tmpfiles.d/par-runner.conf
  systemd-tmpfiles --create /etc/tmpfiles.d/par-runner.conf

  install -d /usr/local/libexec/par-runner
  cat >/usr/local/libexec/par-runner/run-once <<'EOF'
#!/bin/sh
# Runs the GitHub Actions runner for one job with the JIT config the controller delivered. The config goes to the
# runner in an environment variable, so it never appears on a command line, and the file is deleted before the
# job starts.
set -eu
config=/run/par-runner/jitconfig
ACTIONS_RUNNER_INPUT_JITCONFIG=$(cat "$config")
export ACTIONS_RUNNER_INPUT_JITCONFIG
rm -f "$config" /run/par-runner/ready
cd /opt/actions-runner
exec ./run.sh
EOF
  chmod 0755 /usr/local/libexec/par-runner/run-once

  cat >/etc/systemd/system/par-runner.path <<'EOF'
[Unit]
Description=Wait for the JIT runner config from proxmox-actions-runners

[Path]
# The controller writes this empty file once the config is complete: the guest agent creates a file before it writes
# the content, so waiting for the config itself could read it half-written.
PathExists=/run/par-runner/ready
Unit=par-runner.service

[Install]
WantedBy=multi-user.target
EOF
  chmod 0644 /etc/systemd/system/par-runner.path

  cat >/etc/systemd/system/par-runner.service <<'EOF'
[Unit]
Description=GitHub Actions runner for one job (proxmox-actions-runners)
Wants=network-online.target
After=network-online.target
ConditionPathExists=/run/par-runner/jitconfig

[Service]
Type=simple
User=runner
Group=runner
# Hand the config to the runner user before dropping privileges.
ExecStartPre=+/bin/sh -c 'chown -R runner:runner /run/par-runner && chmod 0700 /run/par-runner && chmod 0400 /run/par-runner/jitconfig'
ExecStart=/usr/local/libexec/par-runner/run-once
# One job per VM: whatever happened, the VM powers off and the controller destroys it.
ExecStopPost=+/usr/bin/systemctl --no-block poweroff

[Install]
WantedBy=multi-user.target
EOF
  chmod 0644 /etc/systemd/system/par-runner.service

  systemctl daemon-reload
  # Start it now as well, so the unit is live in the build VM too; workers start it at boot.
  systemctl enable --now par-runner.path
  # The service is started only by the path unit.
  systemctl disable par-runner.service 2>/dev/null || true
}

main "$@"
