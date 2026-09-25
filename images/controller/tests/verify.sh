#!/usr/bin/env bash
# Checks the controller image before cleanup. Run as root by the image build (controller.pkr.hcl).
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
  runuser -u parcon -- parcon version
  check "parcon runs as parcon" runuser -u parcon -- parcon version
  check "parcon is statically linked" bash -c '! ldd /usr/local/bin/parcon'
  check "parcon is a system user" test "$(id -u parcon)" -lt 1000
  check "parcon can't log in" test "$(getent passwd parcon | cut -d: -f7)" = /usr/sbin/nologin
  check "the config directory is parcon's alone" \
    test "$(stat -c '%U %G %a' /etc/proxmox-actions-runners)" = "parcon parcon 700"
  check "parcon.service is valid" systemd-analyze verify /etc/systemd/system/parcon.service
  check "parcon.service runs as parcon" bash -c 'systemctl show -p User --value parcon.service | grep -qx parcon'
  check "parcon.service isn't enabled yet" bash -c '! systemctl is-enabled parcon.service'
  check "qemu-guest-agent is enabled" systemctl is-enabled qemu-guest-agent
  check "unattended-upgrades is enabled" systemctl is-enabled unattended-upgrades
  check "automatic updates are on" grep -q 'Unattended-Upgrade "1"' /etc/apt/apt.conf.d/20auto-upgrades

  if ((failures > 0)); then
    echo "$failures check(s) failed"
    exit 1
  fi
}

main "$@"
