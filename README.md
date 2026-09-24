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

*Planned.* On a Proxmox VE 9 node, as root:

```bash
curl -fsSLO https://github.com/klponce/proxmox-actions-runners/releases/latest/download/install.sh
bash install.sh
```

The installer first runs preflight checks: it must be root on a PVE 9.x node with KVM, a synchronized clock,
enough storage, and outbound HTTPS, among other checks. It then shows the full plan and asks for confirmation.
After that it:

- creates the Proxmox pools, role, user, API token, and ACLs
- creates a small **controller VM** that runs the controller
- builds the **runner template**
- registers the scale set with GitHub

The host itself is barely touched. No packages, services, files, or snippets are added, only Proxmox objects, and
`bash install.sh uninstall` removes all of them. See [docs/install.md](docs/install.md) for the full design.

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
3. For each job, it clones a worker VM from the template and applies the configured hardware spec. After the VM
   boots, the controller writes a just-in-time (JIT) runner config into it through the QEMU guest agent.
4. The runner registers as an ephemeral runner, runs exactly one job, and the VM powers off.
5. The controller sees the VM stop and destroys it. VMs are never reused. A reaper also destroys VMs that outlive
   `maxLifetime` or have no matching runner in GitHub.

The number of workers stays within the configured `minRunners`/`maxRunners`. A warm pool of pre-booted
VMs to cut job start time is *planned*. See [docs/design.md](docs/design.md) for the reasoning and the alternatives
that were considered.

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

| Resource | Default |
| -------- | ------- |
| vCPUs    | 2       |
| Memory   | 8 GiB   |
| Disk     | 14 GiB  |

You can override any of these in the controller config, which the installer writes to
`/etc/proxmox-actions-runners/config.yaml` in the controller VM. *Planned* example:

```yaml
proxmox:
  url: https://pve.example.com:8006/api2/json
  tokenId: github-runners@pve!controller
  tokenSecretFile: /etc/proxmox-actions-runners/pve-token
  tlsFingerprint: "AA:BB:...:FF"   # the host's certificate, pinned by the installer
  node: pve1
  pool: par-runners
  templateVmid: 9000
  storage: local-lvm
  bridge: vmbr0
  linkedClone: true

github:
  configUrl: https://github.com/my-org
  app:
    id: 123456
    installationId: 7890123
    privateKeyFile: /etc/proxmox-actions-runners/github-app.pem

scaleSets:
  - name: proxmox-ubuntu-26.04
    labels: [proxmox-ubuntu-26.04]
    minRunners: 0
    maxRunners: 3    # start low and raise after measuring host contention
    maxLifetime: 6h  # hard limit per worker VM, matching the hosted job limit
    worker:          # omit to use the GitHub-matching defaults
      cores: 4
      memoryMiB: 16384
      diskGiB: 64
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

- **Proxmox VE 9.x** on amd64 with KVM, and root access to one node to run the installer.
- **Clone storage** that supports linked clones: LVM-thin, ZFS, RBD, or qcow2/raw files. Plain LVM and iSCSI need
  `linkedClone: false`.
- **A runner network:** a bridge, or a VLAN on an existing bridge, that is separate from the management network and
  has internet egress. The installer can create an isolated SDN zone instead if you opt in.
- **A GitHub App** installed on the target organization or repository:
  - organization runners need **Self-hosted runners: read & write**
  - repository runners need **Administration: read & write**

The installer creates a privilege-separated API token `par@pve!controller`. Its role, `PARController`, is granted
only on the `par-runners` pool, the target storage, and the runner network:

- on `/pool/par-runners`: `VM.Allocate`, `VM.Clone`, `VM.Config.*`, `VM.PowerMgmt`, `VM.Audit`, and
  `VM.GuestAgent.*`
- on the target storage: `Datastore.AllocateSpace` and `Datastore.Audit`
- on the runner bridge or zone: `SDN.Use`

To build from source, you need **Go** (see `go.mod`) and **Packer**, which CI uses to build the base images.

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
images/runner-base/    Packer build of the runner base image (CI)
images/ubuntu-26.04/   in-guest provisioning scripts for the runner template
deploy/                example config, systemd unit for the controller VM
docs/                  design notes
```

## Security

- Every job runs in a fresh VM that is destroyed afterward, so no state carries over between jobs.
- Self-hosted runners run untrusted code. Be very careful before using them with **public** repositories,
  where anyone who opens a pull request can run code on your infrastructure.
- Put workers on a dedicated bridge or VLAN with internet egress. Firewall off the management network, the Proxmox
  API, and the controller host. Don't bake reusable credentials into the template.
- The Proxmox token is scoped to the runner pool, so the controller can't modify other VMs.
- The GitHub App private key and the Proxmox token are read from files and never logged. JIT configs are
  single-use. They are written straight into the VM that uses them through the guest agent, and are never
  stored in cloud-init snippets or VM config.

## Contributing

See [AGENTS.md](AGENTS.md) for architecture invariants, conventions, and development commands. It applies to
human contributors too.

## License

TBD.
