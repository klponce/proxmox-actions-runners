#!/usr/bin/env bash
# Checks the runner image before cleanup. Run as root by the image build (runner.pkr.hcl).
set -euo pipefail

failures=0
check() {
  local what=$1
  shift
  if "$@" >/dev/null 2>&1; then
    echo "ok    $what"
  else
    echo "FAIL  $what"
    failures=$((failures + 1))
  fi
}

main() {
  check "runner user has UID 1001" test "$(id -u runner)" = 1001
  check "runner user has passwordless sudo" sudo -u runner sudo -n true
  check "runner user is in the docker group" bash -c 'id -nG runner | grep -qw docker'
  check "actions/runner is installed" test -x /opt/actions-runner/run.sh
  check "actions/runner is owned by runner" test "$(stat -c %U /opt/actions-runner/run.sh)" = runner
  check "runner job environment is set" grep -q '^ImageOS=ubuntu26$' /opt/actions-runner/.env
  check "tool cache exists" test -d /opt/hostedtoolcache
  check "qemu-guest-agent is enabled" systemctl is-enabled qemu-guest-agent
  check "par-runner.path is enabled" systemctl is-enabled par-runner.path
  check "par-runner.path is waiting" systemctl is-active par-runner.path
  check "par-runner units are valid" systemd-analyze verify /etc/systemd/system/par-runner.path \
    /etc/systemd/system/par-runner.service
  check "JIT directory is root-only" test "$(stat -c '%U %a' /run/par-runner)" = "root 700"
  check "docker works for the runner user" sudo -u runner docker info
  check "apt background updates are off" grep -q 'Unattended-Upgrade "0"' /etc/apt/apt.conf.d/20auto-upgrades
  check "no systemd ordering cycles at boot" bash -c '! journalctl -b --no-pager | grep -q "Ordering cycle"'
  check "ssh.socket is listening" systemctl is-active ssh.socket

  if ((failures > 0)); then
    echo "$failures check(s) failed"
    exit 1
  fi
}

main "$@"
