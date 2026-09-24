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
2. It long-polls the scale set's message queue for job assignments (the same API ARC uses, so no inbound webhook
   endpoint is needed).
3. For each job, it clones a worker VM from the template, applies the configured hardware spec, and passes a
   just-in-time (JIT) runner config to the VM through cloud-init.
4. The VM boots, the runner registers as an ephemeral runner, runs exactly one job, and exits.
5. The controller sees the job finish and destroys the VM. VMs are never reused.

The number of workers stays within the configured `minRunners`/`maxRunners`. A warm pool of pre-booted
VMs to cut job start time is *planned*.

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

You can override any of these in the controller config. *Planned* example:

```yaml
proxmox:
  url: https://pve.example.com:8006/api2/json
  tokenId: github-runners@pve!controller
  tokenSecretFile: /etc/proxmox-actions-runners/pve-token
  node: pve1
  pool: github-runners
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
    minRunners: 0
    maxRunners: 10
    worker:          # omit to use the GitHub-matching defaults
      cores: 4
      memoryMiB: 16384
      diskGiB: 64
```

## Runner template

Workers are cloned from an Ubuntu 26.04 template built with [Packer](https://www.packer.io/) from
`images/ubuntu-26.04/`. The goal is parity with GitHub's own
[`actions/runner-images`](https://github.com/actions/runner-images) Ubuntu image: the same `runner` user,
directory layout, preinstalled toolset, and environment variables. The template contains no credentials.
Each clone gets its JIT runner config at first boot via cloud-init.

## Requirements

- **Proxmox VE** with an API token for a user or role limited to the runner pool and target storage. The token
  needs `VM.Allocate`, `VM.Clone`, `VM.Config.*`, `VM.PowerMgmt`, `VM.Audit`, `Datastore.AllocateSpace`,
  `Datastore.Audit`, and `SDN.Use` on the bridge.
- **A GitHub App** installed on the target organization or repository:
  - organization runners need **Self-hosted runners: read & write**
  - repository runners need **Administration: read & write**
- **Go** (see `go.mod`) to build the controller, and **Packer** to build the template.

## Repository layout (planned)

```
cmd/controller/        controller entrypoint
internal/config/       config loading, defaults, validation
internal/github/       scale set listener, JIT configs, GitHub App auth
internal/proxmox/      Proxmox API client (clone, configure, start, destroy, list)
internal/controller/   reconcile loop and worker lifecycle
images/ubuntu-26.04/   Packer template, cloud-init, provisioning scripts
deploy/                example config, systemd unit
docs/                  design notes
```

## Security

- Every job runs in a fresh VM that is destroyed afterward, so no state carries over between jobs.
- Self-hosted runners run untrusted code. Be very careful before using them with **public** repositories,
  where anyone who opens a pull request can run code on your infrastructure. Put workers on an isolated network.
- The GitHub App private key and the Proxmox token are read from files and never logged. JIT configs are
  single-use and are only passed to the VM that uses them.

## Contributing

See [AGENTS.md](AGENTS.md) for architecture invariants, conventions, and development commands. It applies to
human contributors too.

## License

TBD.
