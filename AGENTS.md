# AGENTS.md

Guidance for coding agents (and humans) working in this repository.

## Project overview

`proxmox-actions-runners` is a Go controller that serves GitHub Actions jobs with ephemeral VMs on Proxmox VE.
It is modeled on Actions Runner Controller (ARC): it registers a GitHub **runner scale set**, long-polls it for job
assignments, and for each job clones a **worker VM** from an Ubuntu 26.04 **template** that mirrors GitHub-hosted
runners. The worker runs exactly one job with a JIT runner config and is then destroyed.

The project is in early development. Most of the layout below is planned. When you add a component, follow this
document. If you change the design, update this document.

### Glossary

- **Runner scale set**: a GitHub-side group of runners that workflows target by name. The controller receives job
  assignments through its message queue API, the same one ARC uses.
- **JIT config**: a single-use, just-in-time runner registration config from GitHub. It registers one ephemeral
  runner.
- **Template**: the Proxmox VM template built by Packer from `images/ubuntu-26.04/`.
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
   Every step must be idempotent and safe to retry.
3. **Crash-safe.** The controller holds no state that it can't rebuild. On restart it rebuilds its view from
   Proxmox VM tags and metadata plus GitHub, then cleans up orphans such as VMs with no runner, runners with no VM,
   and half-created clones.
4. **Only touch what we own.** Every managed VM is tagged, for example with `par-managed` and a scale set tag. The
   controller must never modify or delete an untagged VM, and must never modify the template.
5. **GitHub-matching defaults.** Default worker hardware is 2 vCPU, 8 GiB RAM, and a 14 GiB disk, matching
   `ubuntu-latest` for private repositories. The defaults are defined in exactly one place (`internal/config`)
   and can be overridden per controller and per scale set.
6. **Secrets never leak.** The GitHub App private key, the Proxmox API token, installation tokens, and JIT configs
   must never be logged, put in error messages, or stored in VM notes or tags. Pass JIT configs to the VM only
   through cloud-init.

## Repository layout (planned)

```
cmd/controller/        main entrypoint: flags, config load, wiring, signal handling
internal/config/       config schema, defaults, validation
internal/github/       GitHub App auth, scale set registration, message listener, JIT configs
internal/proxmox/      Proxmox API client wrapper: clone, configure, start, stop, destroy, list by tag
internal/controller/   reconcile loop, worker lifecycle state machine
images/ubuntu-26.04/   Packer template, cloud-init, provisioning scripts
deploy/                example config, systemd unit
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
packer validate images/ubuntu-26.04
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
- Keep dependencies few and well-maintained.

## Template guidelines (`images/ubuntu-26.04/`)

- Aim for parity with the Ubuntu image in [`actions/runner-images`](https://github.com/actions/runner-images):
  the `runner` user with passwordless sudo, the same directory layout, toolset, and environment variables.
- The template must contain no credentials, runner registration, or machine-specific identity. Reset the
  machine-id and SSH host keys, and clean cloud-init state before converting to a template.
- The runner starts at first boot from the JIT config delivered by cloud-init, runs one job, and then the VM shuts
  down, which signals completion.

## Change hygiene

- Update `README.md` and this file when you change the architecture, config schema, defaults, or commands.
- Use [Conventional Commits](https://www.conventionalcommits.org/) (`feat:`, `fix:`, `docs:`, `chore:`, and so on).
- Keep changes focused. Don't mix refactors with behavior changes.
