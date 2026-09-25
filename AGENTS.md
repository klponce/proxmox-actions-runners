# AGENTS.md

Guidance for coding agents (and humans) working in this repository.

## Project overview

`proxmox-actions-runners` is a Go controller that serves GitHub Actions jobs with ephemeral VMs on Proxmox VE.
It is modeled on Actions Runner Controller (ARC): it registers a GitHub **runner scale set**, long-polls it for the
number of runners GitHub wants, and keeps that many **worker VMs** running, each cloned from a lean Ubuntu 26.04
**template**. Each worker runs exactly one job with a JIT runner config and is then destroyed. Like ARC's runner
image, the template carries the runner, Docker, and a few basics; workflows bring their own toolchains with `setup-*`
actions. Matching the preinstalled toolset of GitHub-hosted runners is out of scope.

The project supports exactly one setup: a single standalone Proxmox VE 9 node, installed by `install/install.sh`.
Don't add support for clusters, containers or Kubernetes, other hypervisors, or per-scale-set networks. The README's
*Limitations* section lists what is out of scope and why.

The project is in early development. Most of the layout below is planned. When you add a component, follow this
document. If you change the design, update this document. [docs/design.md](docs/design.md) explains the reasoning
behind the invariants and the alternatives that were rejected.

### Glossary

- **Runner scale set**: a GitHub-side group of runners that workflows target by name. The controller receives job
  assignments through its message queue API, the same one ARC uses.
- **JIT config**: a single-use, just-in-time runner registration config from GitHub. It registers one ephemeral
  runner. Treat it as a credential until it is used.
- **`actions/scaleset`**: GitHub's Go client for the scale set APIs (the non-Kubernetes counterpart to ARC). The
  controller uses it for scale set registration, the message session, and JIT configs.
- **Template**: a versioned, immutable Proxmox VM template imported from the runner image (`images/runner/`), which
  CI builds with Packer and publishes with each release. Nothing is built on the node.
- **Controller VM**: the VM the installer creates to run the `parcon` controller. It lives in the `par-system` pool,
  outside the token's reach.
- **Worker network**: the isolated Proxmox SDN VNet (`parnet` in the simple zone `parzone`) that workers attach to.
  It has no uplink, no host IP, and no host SNAT or DHCP.
- **Gateway VM**: the VM the installer creates in `par-system` with one NIC on the LAN and one on the worker network.
  It is the worker network's only way out: it provides DHCP, DNS, and NAT to the internet, and blocks the LAN.
- **Installer**: `install/install.sh`, run as root on a Proxmox VE 9 node. See [docs/install.md](docs/install.md).
- **Worker**: a VM cloned from the template to run one job.
- **Linked clone**: a copy-on-write clone of the template. It is fast and saves space. Full clones are also
  supported.

## Public repository: no private data

This project is published in a **public** repository. Everything committed, including history, is visible to
everyone.

- Never commit private data or PII. This includes credentials, tokens, private keys, real hostnames, IP addresses,
  domain names, Proxmox node or cluster names, GitHub org or repo names from real deployments, usernames, email
  addresses, and personal notes.
- In examples, docs, and tests, use placeholders such as `example.com`, `pve1`, `my-org`, the
  `192.0.2.0/24` documentation range, and obviously fake IDs.
- Keep private data (local configs, credentials, notes, scratch files) in `.agents/` or in another location listed
  in `.gitignore`. Before you add a new private location, add it to `.gitignore` first.
- Before committing, check the staged diff (`git diff --cached`) for anything private. If something private was
  committed, don't just delete it in a new commit. Stop and tell the maintainer, because it has to be removed from
  history and any credentials must be rotated.

## Architecture invariants

Keep these true. If a change needs to break one, discuss it first.

1. **One VM, one job.** Runners are always ephemeral. A worker VM is destroyed after its job finishes or fails and
   is never reused or returned to a pool after running a job.
2. **Reconciliation, not event scripts.** The controller compares desired state (GitHub's desired runner count,
   clamped to `minRunners`/`maxRunners`) with actual state (managed VMs in Proxmox and runners in GitHub) and
   converges. Every step must be idempotent and safe to retry, with backoff.
3. **Crash-safe.** The controller holds no state that it can't rebuild. On restart it rebuilds its view from
   Proxmox VM tags and metadata plus GitHub, then cleans up orphans such as VMs with no runner, runners with no VM,
   and half-created clones. A half-created clone is destroyed, not repaired.
4. **Bounded lifetime.** Every worker has a hard `maxLifetime` (6 hours by default). The reaper destroys any managed
   VM past that limit, or with no matching GitHub runner past a grace period, whatever state it is in.
5. **Only touch what we own.** Every managed VM is tagged, for example with `par-managed` and a scale set tag, and
   lives in the runner pool. The controller must never modify or delete an untagged VM. It must never modify an
   existing template: a new runner image is imported as a new template instead, and the controller deletes an old
   one only once no worker uses it. The installer's smoke-test clones (`par-build`, in the reserved VMIDs) belong to
   the installer, which destroys them, including ones a failed run left. The Proxmox token's ACLs enforce the same
   limit, and the controller and gateway VMs sit in `par-system`, outside the token's reach.
6. **GitHub-matching defaults.** Default worker hardware is 2 vCPU, 8 GiB RAM, and 14 GiB of free disk space,
   matching `ubuntu-latest` for private repositories. The free space is added on top of the template's disk size,
   because a clone's disk can grow but never shrink below its template's. The defaults are defined in exactly one
   place (`internal/config`) and can be overridden per controller and per scale set.
7. **Secrets never leak.** The GitHub App private key, the Proxmox API token, installation tokens, and JIT configs
   must never be logged, put in error messages, or stored in VM notes, tags, config, or cloud-init snippets. Pass
   JIT configs to the running VM only through the QEMU guest agent. Cloud-init carries only non-secret settings.
8. **Minimal host footprint.** The installer changes the Proxmox host only through `pveum`, `qm`, `pvesh`, and
   `pvesm`, and only to create objects it tags or names as ours: pools, role, user, token, ACLs, VMs, and the worker
   network's SDN zone and VNet. It installs no packages, services, binaries, or snippets, and doesn't edit host
   network or storage config by hand. The worker network has no host IP, SNAT, or DHCP, so the host never routes
   worker traffic. Every host change must appear in the plan the installer prints, and `install.sh uninstall` must
   remove it. Everything else runs inside the controller and gateway VMs.
9. **Workers are isolated.** Workers attach only to the worker network. Their only way out is the gateway VM, which
   allows DHCP, DNS, and outbound internet traffic and drops everything else, including the LAN, the Proxmox host,
   the controller VM, and private and link-local ranges. The controller reaches workers only through the Proxmox API
   and the guest agent, never over the network.

## Repository layout (planned)

```
cmd/parcon/            `parcon` binary: `run`, `check`, `github app create|import|wait-installation`,
                       `github scaleset delete`; flags, wiring, signals
internal/config/       config schema, defaults, validation
internal/github/       thin wrapper over actions/scaleset: GitHub App auth, scale set, message session, JIT configs
internal/proxmox/      Proxmox API client wrapper: clone, configure, start, stop, destroy, list by tag, guest-agent writes
internal/controller/   reconcile loop, worker lifecycle state machine, reaper
internal/metrics/      Prometheus metrics
internal/vmtags/       the Proxmox tags that hold the controller's state, shared by the controller and `parcon check`
install/               install.sh (the only thing that runs on the Proxmox host) and its bats tests
images/common/         what every image build shares: the Ubuntu and plugin pins, base and cleanup steps, boot test
images/controller/     Packer (qemu builder): the controller VM image with parcon, shipped as a release asset
images/gateway/        Packer (qemu builder): the gateway VM image (nftables, dnsmasq), shipped as a release asset
images/runner/         Packer (qemu builder): the runner template image, shipped as a release asset
deploy/                example config, systemd unit for the controller VM
.devcontainer/         development container with every tool below, at pinned versions
site/                  GitHub Pages helper page for the GitHub App manifest flow (static, no third-party scripts)
docs/                  design notes
```

`internal/controller` depends on small interfaces, not on the concrete Proxmox or GitHub clients, so that tests
can use fakes.

## Development environment

All tools run in the dev container defined in `.devcontainer/`: Go, golangci-lint, Packer with QEMU, ShellCheck,
Bats, and Perl's JSON module, at pinned versions. Don't install toolchains on the host. If a tool is missing, add it
to `.devcontainer/Dockerfile` with a pinned version and checksum. The container gets the host's `/dev/kvm` so Packer
can build images at native speed, so the host needs KVM.

VS Code and other editors that support dev containers pick it up directly. From a terminal, use the
[devcontainer CLI](https://github.com/devcontainers/cli):

```bash
devcontainer up --workspace-folder .
devcontainer exec --workspace-folder . go test ./...
```

The workspace is mounted at its host path. In a git worktree, also mount the main repository's `.git` directory at
its host path, because the worktree's `.git` file points there. Without it, git and Go's VCS stamping fail:

```bash
devcontainer up --workspace-folder . \
  --mount "type=bind,source=$(git rev-parse --git-common-dir),target=$(git rev-parse --git-common-dir)"
```

## Development commands

Run these inside the dev container.

```bash
go build ./...
go test ./...
go vet ./...
gofmt -l .                                # must print nothing
golangci-lint run
test/integration/run.sh                   # needs SSH to a throwaway Proxmox node; see test/integration/README.md
packer fmt -check -recursive images
packer validate images/runner
packer validate images/gateway
CGO_ENABLED=0 go build -o bin/parcon ./cmd/parcon && packer validate -var parcon_binary=bin/parcon images/controller
shellcheck images/common/*.sh images/*/scripts/*.sh images/*/tests/*.sh images/gateway/par-gateway-configure \
  test/integration/*.sh test/integration/node/*.sh install/install.sh install/tests/helpers.bash
bats install/tests
```

Run the build, test, vet, and format checks before you consider a change done. When you change an image, also
build it. Each build runs its scripts twice (they must be idempotent) and checks the result; the runner build also
runs the one-job flow with a stand-in runner. The boot test then boots the finished image the way Proxmox first
boots a VM made from it and checks the console for failed units, ordering cycles, and each image's own lines. A
build needs `/dev/kvm` and 2 to 4 GiB of free memory. Run one build at a time:

```bash
packer init images/runner && packer build -var version=dev images/runner
images/common/boot-test.sh output-runner/par-runner-dev.qcow2 'Started.*par-runner.path'

packer init images/gateway && packer build -var version=dev images/gateway
images/common/boot-test.sh output-gateway/par-gateway-dev.qcow2 'Finished.*nftables.service' 'Started.*dnsmasq.service'

CGO_ENABLED=0 go build -ldflags "-X main.version=dev" -o bin/parcon ./cmd/parcon
packer init images/controller && packer build -var version=dev -var parcon_binary=bin/parcon images/controller
images/common/boot-test.sh output-controller/parcon-dev.qcow2
```

Run the integration suite when you change how the controller talks to Proxmox: the fakes only check what the code
expects, and the suite has already caught a missing privilege (`Pool.Audit`) that every unit test passed with.

## Go conventions

- Use the standard Go layout. Put non-exported packages under `internal/`.
- Make `context.Context` the first parameter of anything that does I/O or blocks, and respect cancellation.
- Wrap errors with `fmt.Errorf("...: %w", err)`. Don't log an error and also return it.
- Use `log/slog` for structured logging. Include `scaleSet`, `vmid`, `jobId`, and `runnerName` attributes where
  relevant.
- Prefer table-driven tests. Unit tests must not call real Proxmox or GitHub APIs. Put those tests behind the
  `integration` build tag.
- Proxmox tasks (clone, destroy, and others) are asynchronous. Always wait for the task to finish and check its exit
  status. Don't assume success when the call returns.
- Take new VMIDs only from the configured `vmidRange`, and treat a "VMID already exists" clone failure as a retryable
  race. `/cluster/nextid` can hand the same ID to two clones started at once.
- Find the current template by its tags (`par-template` and the newest version tag), never by a fixed VMID.
- Keep dependencies few and well-maintained. Pin `actions/scaleset` and keep `internal/github` thin, because its
  interfaces may still change during the preview. Only `internal/github` imports it; everything else uses that
  package's types.
- A JIT config is a credential: keep it in `github.JITConfig`, whose `String`, `GoString`, and `LogValue` redact
  it, and read it with `Encoded()` only to hand it to the guest agent.
- Expose Prometheus metrics for queue depth, desired vs. actual workers, clone and boot latency, VM count by state,
  reaped VMs, and API failures. A job that stays queued is the main signal operators need. *Deferred past v0.1:*
  `parcon` will serve `/metrics`, `/healthz`, and `/readyz` over plain HTTP on `127.0.0.1:9465` only, and nginx in
  the controller VM will serve them over HTTPS on port 9464. Don't make `parcon` listen on the LAN or handle TLS
  itself.
- Authenticate to GitHub only as a GitHub App (Client ID, installation ID, private key). Don't add PAT support.
- Never log the manifest-flow code. It can be exchanged for the App's private key until it is used or expires.
  `parcon github app` commands read the code and keys from stdin, never from arguments.
- `github.app` is optional when the config is parsed, because the installer checks Proxmox before it creates the
  App. A command that talks to GitHub as the App (`parcon run`, `parcon check github`) calls
  `(*config.Config).RequireGitHubApp` first; one that needs only public GitHub data, like `parcon check template`,
  doesn't.

## Installer guidelines (`install/install.sh`)

See [docs/install.md](docs/install.md) for the full design.

- Keep invariant 8. If a feature seems to need a new host change, put it in the controller VM instead, or discuss it
  first.
- Preflight checks come first and change nothing. They include root, Proxmox VE 9.x, amd64, KVM, standalone node,
  clock sync, tools, storage, space, the LAN bridge, SDN support, worker subnet overlap, and name or ID clashes. Add a
  check whenever a later step could fail on host state.
- Use Bash with `set -euo pipefail`, put the body in `main`, and call it on the last line so a truncated
  `curl | bash` download can't run.
- Use only tools that ship with Proxmox VE 9. Parse JSON with `perl -MJSON`, not `jq`.
- Send every command that changes the host through the `change` helper, which logs it and honors `--dry-run`.
  Never pass a secret as an argument, so the log never holds one. (Not `run`: that name belongs to bats.)
- Every step checks what already exists before it creates anything, so a re-run continues or upgrades.
- Secrets never touch the host's disk or a command line. Capture them in variables and send them into the
  controller VM with `qm guest exec --pass-stdin`.
- Download only release assets whose SHA-256 is embedded in the script, into a temp directory removed by an `EXIT`
  trap.

## Runner image guidelines (`images/runner/`)

- Keep it lean, like ARC's runner image: the `runner` user with passwordless sudo, the runner, Docker, `git`, and a
  few basics that `actions/checkout` and the `setup-*` actions need. Keep the directory layout and environment
  variables of the Ubuntu image in [`actions/runner-images`](https://github.com/actions/runner-images), so actions
  that look for them work, but don't add its toolset. Users who need more extend the Packer build.
- The template must contain no credentials, runner registration, or machine-specific identity. Reset the
  machine-id and SSH host keys, and clean cloud-init state before converting to a template.
- Scripts run as root in the Packer build VM. They must be idempotent and exit non-zero on failure.
- Keep the image's disk small. Every worker's disk is the template's size plus `freeDiskGiB`, and the root
  filesystem must grow to fill the disk at first boot. The compressed image must stay well under GitHub's 2 GiB
  release-asset limit.
- Pin the `actions/runner` version and its SHA-256 (`runner_version` and `runner_sha256` in `runner.pkr.hcl`) and
  disable runner auto-update. GitHub stops accepting a runner that doesn't update itself 30 days after a newer
  release, so each runner release needs a new runner image, imported by `install.sh upgrade`. The controller logs a
  warning 7 days after a release its template lacks and an error after 21 (`parcon check template` shows the same).
- Scripts run in order: `images/common/base.sh` (system upgrade and `qemu-guest-agent`, shared with the other
  images), `scripts/base.sh` (the `runner` user, automatic updates off),
  `scripts/10-runner.sh` (the pinned runner in `/opt/actions-runner`, its job environment in `.env`, and
  `/home/runner/work`), `scripts/20-par-runner.sh` (the one-job units), `scripts/30-minimal-tools.sh`, the checks in
  `tests/`, then `images/common/cleanup.sh`, which every image build shares.
- The controller writes the JIT config through the guest agent to `/run/par-runner/jitconfig`
  (`controller.JITConfigPath`), then writes the empty marker `/run/par-runner/ready` (`controller.JITReadyPath`).
  `par-runner.path` waits for the marker, not the config, because the guest agent creates a file before it writes
  the content. `par-runner.service` hands the config to the `runner` user, passes it to `run.sh` in
  `ACTIONS_RUNNER_INPUT_JITCONFIG` (never on a command line), deletes both files, runs one job, and powers the VM
  off when the runner exits for any reason, which signals completion.
- The template must create `/run/par-runner` at boot, root-only (`20-par-runner.sh` uses a `tmpfiles.d` entry): the
  guest agent's file-write can't create directories, so without it every worker fails at the JIT step.
- Runners work in `/home/runner/work` (`github.WorkFolder`), as on GitHub-hosted runners.
- The template contract, which the installer follows when it imports the image and the controller relies on: the
  root disk on `scsi0`, a cloud-init drive (for `ipconfig0`), the guest agent enabled, the VM in the runner pool,
  and the tags `par-managed`, `par-template`, `par-tv-<import time in Unix seconds>`, and
  `par-rv-<actions/runner version>` (`internal/vmtags`). The controller clones the newest `par-tv`, tags each worker
  `par-tpl-<template VMID>`, and destroys older templates that no worker references.
- Templates go in the reserved VMIDs, the last `config.ReservedVMIDs` IDs of the range, never in the worker IDs
  (`config.VMIDRange.Workers`). A new template reports `template: 0` for about 10s, and in the worker IDs it would
  look like a half-created worker clone and be destroyed.

## Gateway and controller image guidelines (`images/gateway/`, `images/controller/`)

- Both start from the same pinned Ubuntu 26.04 cloud image and build VM as the runner (`images/common/ubuntu.pkr.hcl`,
  linked into each image directory) and run `images/common/base.sh` (the guest agent) and
  `images/common/auto-updates.sh` (`unattended-upgrades`) first.
  Unlike workers, these VMs are long-lived and patch themselves. They hold no machine identity either:
  `images/common/cleanup.sh` runs last.
- Keep their disks small: 6 GiB, both VMs' disk size. A built image holds about 2.5 GiB, but a kernel update needs
  room for two kernels and a new initramfs, which 4 GiB can't hold.
- The gateway holds no secrets and no state beyond `/etc/par-gateway/config`. `par-gateway-configure` renders
  everything else from it, and running it again with the same input changes nothing. The worker NIC is always
  `net1` (Proxmox's `net1`, named by its PCI slot), so the rules never depend on MACs.
- The controller image, the installer, and `parcon` all follow the *Controller VM contract* in
  [docs/install.md](docs/install.md): the `parcon` user, `/usr/local/bin/parcon`, `parcon.service`
  ([deploy/parcon.service](deploy/parcon.service), installed but not enabled by the image), and the modes of
  `/etc/proxmox-actions-runners` and its files. Change it there, not here.

## Change hygiene

- Update `README.md` and this file when you change the architecture, config schema, defaults, or commands.
- Use [Conventional Commits](https://www.conventionalcommits.org/) (`feat:`, `fix:`, `docs:`, `chore:`, and so on).
- Keep changes focused. Don't mix refactors with behavior changes.
