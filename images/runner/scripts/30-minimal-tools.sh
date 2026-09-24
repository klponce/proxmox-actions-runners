#!/usr/bin/env bash
# The minimal toolset (`--template minimal`): what most workflows need beyond the runner, above all a native Docker
# daemon the runner user can use without sudo. The full toolset for parity with GitHub-hosted runners builds on it.
set -euo pipefail

main() {
  export DEBIAN_FRONTEND=noninteractive
  apt-get update
  apt-get -y install --no-install-recommends \
    build-essential \
    docker-buildx \
    docker-compose-v2 \
    docker.io \
    git \
    git-lfs \
    gnupg \
    jq \
    openssh-client \
    python3 \
    python3-pip \
    python3-venv \
    rsync \
    unzip \
    wget \
    xz-utils \
    zip \
    zstd

  usermod -aG docker runner
  systemctl enable docker.service containerd.service
}

main "$@"
