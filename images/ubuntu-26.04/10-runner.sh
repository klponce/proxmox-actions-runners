#!/usr/bin/env bash
# Installs the pinned GitHub Actions runner in /opt/actions-runner. Self-updates are disabled on the scale set, so
# a new runner release means bumping the version here and rebuilding the template.
set -euo pipefail

RUNNER_VERSION=2.337.0
RUNNER_SHA256=70920811a4f8ad4328818682bca5c6469c1c942fab52448868071d0063816613
RUNNER_DIR=/opt/actions-runner
# Global, not local: the EXIT trap runs after main returns.
TMP_DIR=""

main() {
  export DEBIAN_FRONTEND=noninteractive

  if [[ -x $RUNNER_DIR/run.sh && $(cat "$RUNNER_DIR/.par-version" 2>/dev/null) == "$RUNNER_VERSION" ]]; then
    echo "actions/runner $RUNNER_VERSION is already installed"
  else
    TMP_DIR=$(mktemp -d)
    trap 'rm -rf "$TMP_DIR"' EXIT
    curl -fsSL --retry 5 -o "$TMP_DIR/runner.tar.gz" \
      "https://github.com/actions/runner/releases/download/v$RUNNER_VERSION/actions-runner-linux-x64-$RUNNER_VERSION.tar.gz"
    echo "$RUNNER_SHA256  $TMP_DIR/runner.tar.gz" | sha256sum -c -
    rm -rf "$RUNNER_DIR"
    mkdir -p "$RUNNER_DIR"
    tar -xzf "$TMP_DIR/runner.tar.gz" -C "$RUNNER_DIR"
    echo "$RUNNER_VERSION" >"$RUNNER_DIR/.par-version"
  fi

  apt-get update
  "$RUNNER_DIR/bin/installdependencies.sh"

  # Where the setup-* actions cache tool versions, as on GitHub-hosted runners.
  install -d -o runner -g runner /opt/hostedtoolcache /home/runner/work

  # The runner passes the variables in .env to every job.
  cat >"$RUNNER_DIR/.env" <<'EOF'
LANG=C.UTF-8
ImageOS=ubuntu26
AGENT_TOOLSDIRECTORY=/opt/hostedtoolcache
RUNNER_TOOL_CACHE=/opt/hostedtoolcache
EOF
  chown -R runner:runner "$RUNNER_DIR"
}

main "$@"
