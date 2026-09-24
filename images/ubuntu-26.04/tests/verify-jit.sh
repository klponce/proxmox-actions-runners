#!/usr/bin/env bash
# Exercises the one-job flow of 20-par-runner.sh without GitHub: a stand-in run.sh records what it received, and
# power-off is replaced by a no-op. Everything is restored afterwards. Run as root by the local test build.
set -euo pipefail

# write_file MODE PATH writes stdin to PATH with MODE, atomically, so a rerun or an interrupted run never leaves a
# partial file. (install from /dev/stdin fails depending on how bash implements the heredoc.)
write_file() {
  local mode=$1 path=$2 tmp
  tmp=$(mktemp "$path.XXXXXX")
  cat >"$tmp"
  chmod "$mode" "$tmp"
  mv -f "$tmp" "$path"
}

RUNNER_DIR=/opt/actions-runner
CONFIG=/run/par-runner/jitconfig
RESULT=/tmp/par-jit-test

main() {
  mv "$RUNNER_DIR/run.sh" "$RUNNER_DIR/run.sh.real"
  write_file 0755 "$RUNNER_DIR/run.sh" <<EOF
#!/bin/sh
{
  echo "user=\$(id -un)"
  echo "config=\$ACTIONS_RUNNER_INPUT_JITCONFIG"
  if [ -e $CONFIG ]; then echo "file=present"; else echo "file=deleted"; fi
} >$RESULT
EOF
  chown runner:runner "$RUNNER_DIR/run.sh"
  install -d /etc/systemd/system/par-runner.service.d
  write_file 0644 /etc/systemd/system/par-runner.service.d/test.conf <<'EOF'
[Service]
ExecStopPost=
ExecStopPost=/bin/true
EOF
  systemctl daemon-reload
  trap restore EXIT

  rm -f "$RESULT"
  # What the controller does through the guest agent: write the file as root.
  printf 'test-jit-config' >"$CONFIG"

  for _ in $(seq 60); do
    [[ -s $RESULT ]] && break
    sleep 1
  done
  cat "$RESULT"
  grep -qx 'user=runner' "$RESULT"
  grep -qx 'config=test-jit-config' "$RESULT"
  grep -qx 'file=deleted' "$RESULT"
  echo "ok    the JIT config reached the runner as the runner user, and the file was deleted first"
}

restore() {
  mv -f "$RUNNER_DIR/run.sh.real" "$RUNNER_DIR/run.sh"
  rm -rf /etc/systemd/system/par-runner.service.d "$RESULT" "$CONFIG"
  systemctl daemon-reload
  systemctl reset-failed par-runner.service 2>/dev/null || true
  # Leave the path unit freshly started and waiting, as it is on a worker's first boot.
  systemctl restart par-runner.path
  # tmpfiles owns the directory; put its root-only mode back after the service handed it to the runner user.
  chown root:root /run/par-runner
  chmod 0700 /run/par-runner
}

main "$@"
