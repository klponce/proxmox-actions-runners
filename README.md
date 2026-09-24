# proxmox-actions-runners

A controller that runs GitHub Actions jobs on ephemeral, full-sized virtual machines on your own
[Proxmox VE](https://www.proxmox.com/en/proxmox-virtual-environment) cluster.

It works like [Actions Runner Controller (ARC)](https://github.com/actions/actions-runner-controller),
but each job gets its own VM instead of a Kubernetes pod. Every VM is cloned from an Ubuntu 26.04 template
that is built to resemble a GitHub-hosted runner as closely as possible, so workflows written for
`ubuntu-latest` should run unchanged.

> **Status: early development.** Nothing here is usable yet. This README describes the intended design.
> Sections marked *planned* describe features that don't exist yet.

## Why VMs?

ARC runs jobs in containers. Many workflows assume a real machine, which containers don't give them:

- a full init system (`systemd`) and services such as a native Docker daemon, without Docker-in-Docker
- `sudo`, kernel modules, loop devices, KVM, and other privileged operations
- the same OS image, toolset, and filesystem layout as GitHub-hosted runners

A fresh VM per job gives you all of this, with hypervisor-level isolation between jobs, on hardware you
control.

## Install

*Planned.* On a standalone Proxmox VE 9 node, as root:

```bash
curl -fsSLO https://github.com/klponce/proxmox-actions-runners/releases/latest/download/install.sh
bash install.sh
```

The installer first runs preflight checks: it must be root on a PVE 9.x node with KVM, a synchronized clock,
enough storage, and outbound HTTPS, among other checks. It then shows the full plan and asks for confirmation.
After that it:

- creates the Proxmox pools, role, user, API token, and ACLs
- creates an isolated **worker network** (a Proxmox SDN VNet with no uplink) and a small **gateway VM** that is its
  only way out to the internet
- creates a small **controller VM** that runs the controller
- creates the **GitHub App**: it prints a link that you open in a browser on any device, then you paste one code
  back, so the host itself needs no browser
- builds the **runner template**
- registers the scale set with GitHub

The host itself is barely touched. No packages, services, files, or snippets are added, only Proxmox objects, and
`bash install.sh uninstall` removes all of them. The host doesn't route worker traffic, so the design doesn't
depend on how your LAN, firewall, or host networking is set up. See [docs/install.md](docs/install.md) for the full
design.

## How it works

```
        GitHub                             Controller                          Proxmox VE
 ┌──────────────────┐   long-poll     ┌─────────────────────┐   API    ┌──────────────────────────┐
 │ Runner scale set │◄────────────────┤ listener            │          │ template (Ubuntu 26.04)  │
 │  job assigned    ├────────────────►│ reconcile loop      ├─────────►│   │ clone                │
 └──────────────────┘  JIT config     │                     │          │   ▼                      │
          ▲                           └─────────────────────┘          │ worker VM ── runs 1 job  │
          │               runner registers with JIT config             │   │ then destroyed       │
          └────────────────────────────────────────────────────────────┤   ▼                      │
                                                                       └──────────────────────────┘
```

1. The controller registers a **runner scale set** with GitHub. Workflows target it with `runs-on: <scale-set-name>`.
2. It long-polls the scale set's message queue for job assignments. This uses GitHub's
   [`actions/scaleset`](https://github.com/actions/scaleset) client, which talks to the same API as ARC, so no
   inbound webhook endpoint is needed.
3. GitHub reports how many runners the scale set needs. The controller keeps that many workers running, clamped to
   `minRunners`/`maxRunners`. For each new worker, it clones a VM from the template, applies the configured hardware
   spec, and attaches it to the worker network. After the VM boots, the controller writes a just-in-time (JIT)
   runner config into it through the QEMU guest agent.
4. The runner registers as an ephemeral runner. GitHub assigns it exactly one job, and the VM powers off afterward.
5. The controller sees the VM stop and destroys it. VMs are never reused. A reaper also destroys VMs that outlive
   `maxLifetime` or have no matching runner in GitHub.

Setting `minRunners` above zero keeps that many idle workers booted and registered, so jobs start without waiting
for a clone. See [docs/design.md](docs/design.md) for the reasoning and the alternatives that were considered.

### Compared with ARC

|                  | ARC                             | proxmox-actions-runners          |
| ---------------- | ------------------------------- | -------------------------------- |
| Runs on          | Kubernetes                      | Proxmox VE                       |
| Execution unit   | Pod (container)                 | Full VM                          |
| Isolation        | Container / namespace           | Hypervisor                       |
| Runner image     | Container image                 | VM template (Ubuntu 26.04)       |
| Job source       | Runner scale sets               | Runner scale sets                |
| Runner lifecycle | Ephemeral, one job per runner   | Ephemeral, one job per VM        |

## Worker hardware

The default spec matches the standard GitHub-hosted `ubuntu-latest` runner for private repositories:

| Resource | Default     |
| -------- | ----------- |
| vCPUs    | 2           |
| Memory   | 8 GiB       |
| Disk     | 14 GiB free |

GitHub's 14 GB is the free space a job gets, not the size of the disk. Each worker's disk is the template's disk
size plus `freeDiskGiB` (14 by default), because a clone's disk can grow but can't be smaller than its template's.

You can override any of these in the controller config, for all scale sets in a top-level `worker:` block or for one
scale set in its own `worker:` block. The installer writes the config to `/etc/proxmox-actions-runners/config.yaml`
in the controller VM, and `parcon check config` validates it. Example (also in
[`deploy/config.example.yaml`](deploy/config.example.yaml)):

```yaml
proxmox:
  url: https://pve.example.com:8006/api2/json
  tokenId: par@pve!controller
  tokenSecretFile: /etc/proxmox-actions-runners/pve-token
  tlsFingerprint: "AA:BB:...:FF"   # the host's certificate, pinned by the installer
  node: pve1
  pool: par-runners
  storage: local-lvm
  vnet: parnet                     # the worker network the installer creates
  vmidRange: { start: 10000, end: 10999 }
  linkedClone: true                # the template is found by its tags, not a fixed VMID

github:
  configUrl: https://github.com/my-org
  app:
    clientId: Iv23liEXAMPLE0000000   # written by the installer's App setup
    installationId: 7890123
    privateKeyFile: /etc/proxmox-actions-runners/github-app.pem

metrics:
  listen: 127.0.0.1:9465  # nginx serves it on the LAN at https://<controller-ip>:9464/

worker:            # controller-wide; omit to use the GitHub-matching defaults (2 cores, 8 GiB, 14 GiB free)
  cores: 2

scaleSets:
  - name: proxmox-ubuntu-26.04
    labels: [proxmox-ubuntu-26.04]
    runnerGroup: default  # GitHub runner group; repository scale sets must use "default"
    minRunners: 0
    maxRunners: 3    # start low and raise after measuring host contention
    maxLifetime: 6h  # hard limit per worker VM, matching the hosted job limit
    worker:          # overrides the controller-wide `worker:` block, which defaults to the GitHub-matching spec
      cores: 4
      memoryMiB: 16384
      freeDiskGiB: 50  # added on top of the template's disk size
```

## Using it from workflows

Target the scale set by name:

```yaml
jobs:
  build:
    runs-on: proxmox-ubuntu-26.04
```

GitHub has no automatic "hosted, else self-hosted" fallback, because `runs-on` is resolved before the job is
queued. To switch between hosted and self-hosted runners without editing workflows, read the label from a repository
or organization variable:

```yaml
    runs-on: ${{ vars.CI_RUNNER || 'ubuntu-latest' }}
```

Leave `CI_RUNNER` unset to use hosted runners. Set it to the scale set name to use Proxmox. Use separate variables and
scale sets for jobs with different trust levels. For example, keep jobs that build code apart from jobs that hold
deployment secrets.

## Runner template

Workers are cloned from an Ubuntu 26.04 template. The controller builds it on your node: it starts from a small
base image published with each release, then runs the provisioning scripts in `images/ubuntu-26.04/` through the
guest agent. The goal is parity with GitHub's own
[`actions/runner-images`](https://github.com/actions/runner-images) Ubuntu image: the same `runner` user,
directory layout, preinstalled toolset, and environment variables. It also includes `qemu-guest-agent` and a
pinned `actions/runner` release with auto-update disabled. The template contains no credentials. Each clone
receives its JIT runner config through the guest agent after boot. Templates are versioned and immutable. The
controller rebuilds the template weekly and on each runner release, and deletes old versions once no worker uses
them.

## Requirements

- **Proxmox VE 9.x** on a single standalone amd64 node with KVM, and root access to run the installer. See
  *Limitations*.
- **Clone storage** that supports linked clones: LVM-thin, ZFS, RBD, or qcow2/raw files. Plain LVM and iSCSI need
  `linkedClone: false`.
- **A LAN bridge with internet egress** (usually `vmbr0`) for the controller and gateway VMs. Workers never attach
  to it: they get a dedicated network behind the gateway VM, which the installer creates.
- **Proxmox SDN** available, which it is by default on PVE 9 (`ifupdown2` and the `source /etc/network/interfaces.d/*`
  line in `/etc/network/interfaces`).
- **A GitHub organization or repository** where you can create and install a GitHub App. The installer creates the
  App with only the permission it needs:
  - organization runners need **Self-hosted runners: read & write**
  - repository runners need **Administration: read & write**
- **A browser on any device** for the one-time App creation step.

The installer creates a privilege-separated API token `par@pve!controller`. Its role, `PARController`, is granted
only on the `par-runners` pool, the target storage, and the worker network:

- on `/pool/par-runners`:
  - `VM.Allocate` (create, destroy, convert to template) and `VM.Clone`
  - `VM.Config.CPU`, `VM.Config.Memory`, `VM.Config.Disk` (grow the clone's disk), `VM.Config.Network`,
    `VM.Config.Cloudinit`, and `VM.Config.Options` (name and tags)
  - `VM.PowerMgmt` and `VM.Audit`
  - `VM.GuestAgent.Audit` (ping), `VM.GuestAgent.FileWrite` (JIT config), and `VM.GuestAgent.Unrestricted`
    (run the template build scripts)
- on the target storage: `Datastore.AllocateSpace` and `Datastore.Audit`
- on the worker VNet (`/sdn/zones/parzone/parnet`): `SDN.Use`

`VM.GuestAgent.Unrestricted` lets the token run commands as root in any VM in `par-runners`. That is accepted: those
VMs are disposable and already fully under the controller's control, and the token can't reach `par-system`.

`parcon check proxmox` checks the token against this list, which lives in `internal/proxmox/access.go`. Keep the
two in sync.

To build from source, use the dev container in `.devcontainer/`, which has Go, Packer, and every other tool at
pinned versions. See [AGENTS.md](AGENTS.md#development-environment).

## Limitations

These are deliberate. The project supports exactly the setup the installer creates, and nothing else.

- **Single standalone node only.** Proxmox clusters aren't supported, even if workers would run on only one node,
  and the installer refuses to run on a cluster member. The maintainer has no hardware to test clusters.
- **One installation layout.** No container image, Kubernetes deployment, other hypervisors, or controller running
  outside its VM. The repository is public, so you are welcome to fork it for other setups.
- **One worker network.** Every scale set shares the same worker network and runner pool. Keep jobs with different
  trust levels apart with separate scale sets and GitHub runner groups.
- **amd64 only**, and workers run **Ubuntu 26.04 only**.
- **GitHub.com only.** GitHub Enterprise Server may work through `actions/scaleset`, but it isn't tested.
- **Unauthenticated metrics.** The metrics endpoint uses HTTPS with a self-signed certificate and has no
  authentication yet. It exposes no secrets.

## Repository layout (planned)

```
cmd/parcon/            `parcon` binary: controller service, `template build`, `check`
internal/config/       config loading, defaults, validation
internal/github/       scale set listener, JIT configs, GitHub App auth
internal/proxmox/      Proxmox API client (clone, configure, start, destroy, list)
internal/controller/   reconcile loop and worker lifecycle
internal/template/     template build through the guest agent
install/               install.sh and its bats tests
images/controller/     Packer build of the controller VM base image (CI)
images/gateway/        Packer build of the gateway VM image (CI)
images/runner-base/    Packer build of the runner base image (CI)
images/ubuntu-26.04/   in-guest provisioning scripts for the runner template
deploy/                example config, systemd unit for the controller VM
site/                  GitHub Pages helper page for creating the GitHub App
docs/                  design notes
```

## Security

- Every job runs in a fresh VM that is destroyed afterward, so no state carries over between jobs.
- Self-hosted runners run untrusted code. Be very careful before using them with **public** repositories,
  where anyone who opens a pull request can run code on your infrastructure.
- Workers sit on their own network. The gateway VM gives them DHCP, DNS, and outbound internet and drops traffic to
  the LAN, the Proxmox host, the controller VM, and private and link-local ranges. The host doesn't route worker
  traffic. Don't bake reusable credentials into the template.
- The Proxmox token is scoped to the runner pool, so the controller can't modify other VMs.
- The GitHub App's private key goes straight from GitHub into the controller VM and never touches the Proxmox host.
- Metrics are served over HTTPS on the LAN without authentication. Don't expose the controller VM beyond your LAN.
- The GitHub App private key and the Proxmox token are read from files and never logged. JIT configs are
  single-use. They are written straight into the VM that uses them through the guest agent, and are never
  stored in cloud-init snippets or VM config.

## Contributing

See [AGENTS.md](AGENTS.md) for architecture invariants, conventions, and development commands. It applies to
human contributors too.

## License

[Apache License 2.0](LICENSE). It matches ARC and is compatible with the MIT-licensed `actions/scaleset`.
