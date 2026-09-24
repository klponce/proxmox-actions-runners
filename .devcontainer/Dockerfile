# Development environment for proxmox-actions-runners. Every tool from AGENTS.md's "Development commands" is here, so
# nothing needs to be installed on the host. Versions are pinned; bump them together with their checksums.
FROM golang:1.27.1-trixie@sha256:433790e515d27dc6003e847e644cc0af956985cf315c1c58a3b73ee2dd305183

ARG GOLANGCI_LINT_VERSION=2.13.2
ARG GOLANGCI_LINT_SHA256=2277d43b98ec0054280f2ac26b53268bae97682444678a59a657dd565da021d6
ARG PACKER_VERSION=1.16.1
ARG PACKER_SHA256=af38a9e93e4ed1b9ca68206ae969c64c300c82a3dde46a780dfa629f0867f651
ARG USERNAME=dev
ARG USER_UID=1000
ARG USER_GID=1000

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        bats \
        ca-certificates \
        curl \
        git \
        less \
        libjson-perl \
        shellcheck \
        sudo \
        unzip \
    && rm -rf /var/lib/apt/lists/*

RUN set -eux; \
    cd /tmp; \
    curl -fsSLo golangci-lint.tar.gz \
        "https://github.com/golangci/golangci-lint/releases/download/v${GOLANGCI_LINT_VERSION}/golangci-lint-${GOLANGCI_LINT_VERSION}-linux-amd64.tar.gz"; \
    echo "${GOLANGCI_LINT_SHA256}  golangci-lint.tar.gz" | sha256sum -c -; \
    tar -xzf golangci-lint.tar.gz --strip-components=1 -C /usr/local/bin \
        "golangci-lint-${GOLANGCI_LINT_VERSION}-linux-amd64/golangci-lint"; \
    curl -fsSLo packer.zip \
        "https://releases.hashicorp.com/packer/${PACKER_VERSION}/packer_${PACKER_VERSION}_linux_amd64.zip"; \
    echo "${PACKER_SHA256}  packer.zip" | sha256sum -c -; \
    unzip -o packer.zip packer -d /usr/local/bin; \
    rm -f golangci-lint.tar.gz packer.zip; \
    golangci-lint version; \
    packer version

RUN groupadd --gid "${USER_GID}" "${USERNAME}" \
    && useradd --uid "${USER_UID}" --gid "${USER_GID}" --create-home --shell /bin/bash "${USERNAME}" \
    && echo "${USERNAME} ALL=(root) NOPASSWD:ALL" > "/etc/sudoers.d/${USERNAME}" \
    && chmod 0440 "/etc/sudoers.d/${USERNAME}" \
    && mkdir -p "/home/${USERNAME}/go" "/home/${USERNAME}/.cache" \
    && chown -R "${USER_UID}:${USER_GID}" "/home/${USERNAME}/go" "/home/${USERNAME}/.cache"

USER ${USERNAME}
ENV GOPATH=/home/${USERNAME}/go \
    GOCACHE=/home/${USERNAME}/.cache/go-build \
    GOLANGCI_LINT_CACHE=/home/${USERNAME}/.cache/golangci-lint \
    GOTOOLCHAIN=local \
    PATH=/home/${USERNAME}/go/bin:${PATH}
