# AGENTS.md

Guidance for coding agents (and humans) working in this repository.

## Project overview

`proxmox-actions-runners` is a Go controller that serves GitHub Actions jobs with ephemeral VMs on Proxmox VE.
It is modeled on Actions Runner Controller (ARC): it registers a GitHub **runner scale set**, long-polls it for job
assignments, and for each job clones a **worker VM** from an Ubuntu 26.04 **template** that mirrors GitHub-hosted
runners. The worker runs exactly one job with a JIT runner config and is then destroyed.

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
- **Template**: a versioned, immutable Proxmox VM template. The controller builds it from the runner base image by
  running the `images/ubuntu-26.04/` scripts through the guest agent.
- **Controller VM**: the VM the installer creates to run the `parcon` controller. It lives in the `par-system` pool,
  outside the token's reach.
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
2. **Reconciliation, not event scripts.** The controller compares desired state (job assignments and
   `minRunners`/`maxRunners`) with actual state (managed VMs in Proxmox and runners in GitHub) and converges.
   Every step must be idempotent and safe to retry, with backoff.
3. **Crash-safe.** The controller holds no state that it can't rebuild. On restart it rebuilds its view from
   Proxmox VM tags and metadata plus GitHub, then cleans up orphans such as VMs with no runner, runners with no VM,
   and half-created clones. A half-created clone is destroyed, not repaired.
4. **Bounded lifetime.** Every worker has a hard `maxLifetime` (6 hours by default). The reaper destroys any managed
   VM past that limit, or with no matching GitHub runner past a grace period, whatever state it is in.
5. **Only touch what we own.** Every managed VM is tagged, for example with `par-managed` and a scale set tag, and
   lives in the runner pool. The controller must never modify or delete an untagged VM. It must never modify an
   existing template: the template builder creates a new version instead, and an old version is deleted only once
   no worker uses it. The Proxmox token's ACLs enforce the same limit, and the controller VM sits in `par-system`,
   outside the token's reach.
6. **GitHub-matching defaults.** Default worker hardware is 2 vCPU, 8 GiB RAM, and a 14 GiB disk, matching
   `ubuntu-latest` for private repositories. The defaults are defined in exactly one place (`internal/config`)
   and can be overridden per controller and per scale set.
7. **Secrets never leak.** The GitHub App private key, the Proxmox API token, installation tokens, and JIT configs
   must never be logged, put in error messages, or stored in VM notes, tags, config, or cloud-init snippets. Pass
   JIT configs to the running VM only through the QEMU guest agent. Cloud-init carries only non-secret settings.
8. **Minimal host footprint.** The installer changes the Proxmox host only through `pveum`, `qm`, `pvesh`, and
   `pvesm`, and only to create objects it tags or names as ours: pools, role, user, token, ACLs, and VMs. It
   installs no packages, services, binaries, or snippets, and doesn't edit host network or storage config. The only
   exception is opt-in SDN mode. Every host change must appear in the plan the installer prints, and
   `install.sh uninstall` must remove it. Everything else runs inside the controller VM.

## Repository layout (planned)

```
cmd/parcon/            `parcon` binary: `run` (controller service), `template build`, `check`; flags, wiring, signals
internal/config/       config schema, defaults, validation
internal/github/       thin wrapper over actions/scaleset: GitHub App auth, scale set, message session, JIT configs
internal/proxmox/      Proxmox API client wrapper: clone, configure, start, stop, destroy, list by tag, guest-agent writes
internal/controller/   reconcile loop, worker lifecycle state machine, reaper
internal/metrics/      Prometheus metrics
internal/template/     template build: clone base, run provisioning through guest-exec, clean up, convert, smoke test
install/               install.sh (the only thing that runs on the Proxmox host) and its bats tests
images/controller/     Packer (qemu builder, CI only): controller VM base image, shipped as a release asset
images/runner-base/    Packer (qemu builder, CI only): runner base image with qemu-guest-agent, shipped as a release asset
images/ubuntu-26.04/   in-guest provisioning scripts for the runner template, run by `parcon template build`
deploy/                example config, systemd unit for the controller VM
docs/                  design notes
```

`internal/controller` depends on small interfaces, not on the concrete Proxmox or GitHub clients, so that tests
can use fakes.

## Development commands

```bash
go build ./...
go test ./...
go vet ./...
gofmt -l .                                # must print nothing
golangci-lint run
go test -tags integration ./...           # needs real Proxmox + GitHub credentials
packer validate images/controller
packer validate images/runner-base
shellcheck install/install.sh images/ubuntu-26.04/*.sh
bats install/tests
```

Run the build, test, vet, and format checks before you consider a change done.

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
- Keep dependencies few and well-maintained. Pin `actions/scaleset` and keep `internal/github` thin, because its
  interfaces may still change during the preview.
- Expose Prometheus metrics for queue depth, desired vs. actual workers, clone and boot latency, VM count by state,
  reaped VMs, and API failures. A job that stays queued is the main signal operators need.

## Installer guidelines (`install/install.sh`)

See [docs/install.md](docs/install.md) for the full design.

- Keep invariant 8. If a feature seems to need a new host change, put it in the controller VM instead, or discuss it
  first.
- Preflight checks come first and change nothing. They include root, Proxmox VE 9.x, amd64, KVM, quorum, clock
  sync, tools, storage, space, bridges, and name or ID clashes. Add a check whenever a later step could fail on host
  state.
- Use Bash with `set -euo pipefail`, put the body in `main`, and call it on the last line so a truncated
  `curl | bash` download can't run.
- Use only tools that ship with Proxmox VE 9. Parse JSON with `perl -MJSON`, not `jq`.
- Send every mutating command through one `run` helper that honors `--dry-run` and masks secrets in logs.
- Every step checks what already exists before it creates anything, so a re-run continues or upgrades.
- Secrets never touch the host's disk or a command line. Capture them in variables and send them into the
  controller VM with `qm guest exec --pass-stdin`.
- Download only release assets whose SHA-256 is embedded in the script, into a temp directory removed by an `EXIT`
  trap.

## Template guidelines (`images/ubuntu-26.04/`)

- Aim for parity with the Ubuntu image in [`actions/runner-images`](https://github.com/actions/runner-images):
  the `runner` user with passwordless sudo, the same directory layout, toolset, and environment variables.
- The template must contain no credentials, runner registration, or machine-specific identity. Reset the
  machine-id and SSH host keys, and clean cloud-init state before converting to a template.
- Scripts run as root inside the build VM through `guest-exec`. There is no SSH and no network path from the
  controller. They must be idempotent and exit non-zero on failure.
- `qemu-guest-agent` comes from the runner base image (`images/runner-base/`). Pin the `actions/runner` version and
  disable runner auto-update. The controller rebuilds the template on each runner release instead.
- The runner service waits for the JIT config file that the controller writes through the guest agent (for example,
  with a systemd path unit). It runs one job, deletes the file, and powers the VM off, which signals completion.

## Change hygiene

- Update `README.md` and this file when you change the architecture, config schema, defaults, or commands.
- Use [Conventional Commits](https://www.conventionalcommits.org/) (`feat:`, `fix:`, `docs:`, `chore:`, and so on).
- Keep changes focused. Don't mix refactors with behavior changes.
