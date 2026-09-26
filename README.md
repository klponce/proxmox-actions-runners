# proxmox-actions-runners

A controller that runs GitHub Actions jobs on ephemeral, full-sized virtual machines on your own
[Proxmox VE](https://www.proxmox.com/en/proxmox-virtual-environment) cluster.

It works like [Actions Runner Controller (ARC)](https://github.com/actions/actions-runner-controller),
but each job gets its own VM instead of a Kubernetes pod, so jobs can use Docker, `systemd`, and `sudo` as they
would on a real machine. Every VM is cloned from a lean Ubuntu 26.04 template that, like ARC's runner image, holds
the runner, Docker, and a few basics; workflows install the rest with `setup-*` actions.

> **Status: early development.** Nothing here is usable yet. This README describes the intended design.
> Sections marked *planned* describe features that don't exist yet.

## Why VMs?

ARC runs jobs in containers. Many workflows assume a real machine, which containers don't give them:

- a full init system (`systemd`) and services such as a native Docker daemon, without Docker-in-Docker
- `sudo`, kernel modules, loop devices, KVM, and other privileged operations
- the same filesystem layout and environment variables as GitHub-hosted runners, so actions that rely on them work

A fresh VM per job gives you all of this, with hypervisor-level isolation between jobs, on hardware you
control.

## Install

*Planned.* On a standalone Proxmox VE 9 node, as root:

```bash
curl -fsSLO https://github.com/klponce/proxmox-actions-runners/releases/latest/download/install.sh
bash install.sh
```

`install.sh` only downloads the release's `parcon`, checks it against the checksum written into the script, and runs
`parcon install`. `parcon` first runs preflight checks: it must be root on a PVE 9.x node with KVM, a synchronized
clock, enough storage, and outbound HTTPS, among other checks. It then shows the full plan and asks for
confirmation, and changes nothing before that (`--dry-run` stops at the plan). After that it:

- installs itself as `/usr/local/bin/parcon`, with its settings in `/etc/proxmox-actions-runners/settings.yaml`

- creates the Proxmox pools, role, user, API token, and ACLs
- creates an isolated **worker network** (a Proxmox SDN VNet with no uplink) and a small **gateway VM** that is its
  only way out to the internet
- creates a small **controller VM** that runs the controller
- creates your **GitHub App**: it prints a link that you open in a browser on any device, where you choose your
  organization or personal account and create the App on GitHub. You paste one line back, install the App with
  GitHub's own install page, and the installer learns the organization or repository from it. The host itself needs
  no browser
- imports the **runner template** from the runner image published with the release
- registers the scale set with GitHub

`parcon install -h` lists the settings you can choose, such as the bridge, static addresses, the worker subnet, or
`--set worker.memory=16GiB`. If a step fails, fix the cause and run `parcon install` again: it continues where it
stopped.

The host itself is barely touched: `parcon`, its settings, Proxmox objects, and, on a node that is itself a VM, a
udev rule for its NIC. No packages or services are added, and `parcon uninstall` removes all of it. The host
doesn't route worker traffic, so the design doesn't depend on how your LAN, firewall, or host networking is set
up. See [docs/install.md](docs/install.md) for the full design.

## Managing it

`parcon` on the Proxmox host is how you manage the runners. The gateway and controller VMs need no logins: `parcon`
reaches into them through the guest agent for what it does.

| Command | What it does |
| ------- | ------------ |
| `parcon status` | The state of the whole install: host, gateway, controller, GitHub session and jobs, template, workers, settings. `--json` for scripts |
| `parcon config get --all` | Every setting you can change, with its value and default |
| `parcon config set worker.memory 16GiB` | Change a setting. The controller restarts with it; running jobs keep going |
| `parcon config describe worker.memory` | Which values a setting takes, and when a change applies |
| `parcon update` | Update to the newest release, once its signature checks out. `--pre` includes pre-releases |
| `parcon check` | Check the node and the controller. `parcon check network` checks the worker network from a throwaway worker |
| `parcon uninstall` | Remove everything, `parcon` included. The GitHub App stays |

The settings you can change so far are `runners.max`, `worker.cores`, and `worker.memory`.

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

Change them with `parcon config set worker.cores 4` and `parcon config set worker.memory 16GiB` on the Proxmox host.
New workers get the new hardware; running ones finish their jobs as they are.

`parcon` renders the controller's config, `/etc/proxmox-actions-runners/config.yaml` in the controller VM, from its
settings on the host, and rewrites it whenever they change, so don't edit it by hand. The config can do more than
the settings expose yet, such as several scale sets or a per-scale-set `worker:` block. Its format, also in
[`deploy/config.example.yaml`](deploy/config.example.yaml):

```yaml
proxmox:
  url: https://192.0.2.1:8006/api2/json
  tokenId: par@pve!controller
  tokenSecretFile: /etc/proxmox-actions-runners/pve-token
  caCertFile: /etc/proxmox-actions-runners/pve-ca.pem  # the node's CA; omit for a certificate the system trusts
  tlsServerName: pve1.example.com  # a name the API's certificate is issued for, since the URL uses an IP
  node: pve1
  pool: par-runners
  storage: local-lvm
  zone: parzone                    # the SDN zone the installer creates; this is the default
  vnet: parnet                     # the worker network the installer creates
  vmidRange: { start: 10000, end: 10999 }
  linkedClone: true                # the template is found by its tags, not a fixed VMID

github:
  configUrl: https://github.com/my-org
  app:                             # left out until the installer has created the App
    clientId: Iv23liEXAMPLE0000000
    installationId: 7890123
    privateKeyFile: /etc/proxmox-actions-runners/github-app.pem

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

Workers are cloned from an Ubuntu 26.04 template that the installer imports from the runner image published with
each release. CI builds the image with Packer from `images/runner/`, so nothing is built on your node. Like ARC's
runner image, it is deliberately lean: the `runner` user with passwordless sudo, a pinned `actions/runner` release
with auto-update disabled, Docker, `git`, `qemu-guest-agent`, and a few basics. Workflows install toolchains with
`setup-*` actions (`actions/setup-node`, `actions/setup-python`, and so on), as they do on ARC. The directory layout
and environment variables follow GitHub's [`actions/runner-images`](https://github.com/actions/runner-images) so
those actions work, but its preinstalled toolset is not included.

The template contains no credentials. Each clone receives its JIT runner config through the guest agent after boot.
Templates are immutable: `parcon update` imports a newer release's runner image as a new template, the controller
clones the newest one, and it deletes an old template once no worker uses it.

GitHub stops accepting a self-hosted runner that doesn't update itself 30 days after a newer runner release, so
keep up with releases. The controller checks for new `actions/runner` releases and logs a warning when the template
has been behind for 7 days and an error after 21. `parcon status` and `parcon check template` show the same.

## Gateway and controller VMs

`parcon install` creates both VMs from images published with each release, built with Packer from `images/gateway/`
and `images/controller/` on the same Ubuntu 26.04 base as the runner image. Unlike workers, they are long-lived and
install security updates with `unattended-upgrades`.

- The **gateway** image has dnsmasq, nftables, and `par-gateway-configure`, which `parcon` runs through the guest
  agent to set the worker subnet and the addresses to block. `parcon update` replaces the gateway with the new
  release's image.
- The **controller** image has the `parcon` binary in `/usr/local/bin`, the `parcon` system user it runs as, and
  its unit, [`deploy/parcon.service`](deploy/parcon.service). Only `parcon` can read its config and secrets in
  `/etc/proxmox-actions-runners`. `parcon install` enables the unit once the GitHub App exists, and `parcon update`
  has the VM download the new release's `parcon`, checked against the release's signed checksums.

See [docs/install.md](docs/install.md#gateway-and-controller-images) for the details.

## Requirements

- **Proxmox VE 9.x** on a single standalone amd64 node with KVM, and root access to run `parcon`. See
  *Limitations*.
- **Clone storage** that supports linked clones: LVM-thin, ZFS, RBD, or qcow2/raw files. Plain LVM and iSCSI need
  `linkedClone: false`.
- **A LAN bridge with internet egress** (usually `vmbr0`) for the controller and gateway VMs. Workers never attach
  to it: they get a dedicated network behind the gateway VM, which the installer creates.
- **Proxmox SDN** available, which it is by default on PVE 9 (`ifupdown2` and the `source /etc/network/interfaces.d/*`
  line in `/etc/network/interfaces`).
- **A GitHub organization or personal account** where you can create and install a GitHub App: an organization's
  runners serve all its repositories, and a personal account's serve one repository, which GitHub requires. The App
  is created in your account with only the permission it needs:
  - organization runners need **Self-hosted runners: write** (write includes read)
  - repository runners need **Administration: write**
- **A browser on any device** for the one-time App creation step.

The installer creates a privilege-separated API token `par@pve!controller`. Its role, `PARController`, is granted
only on the `par-runners` pool, the target storage, and the worker network:

- on `/pool/par-runners`:
  - `VM.Allocate` (create and destroy) and `VM.Clone`
  - `VM.Config.CPU`, `VM.Config.Memory`, `VM.Config.Disk` (grow the clone's disk), `VM.Config.Network`,
    `VM.Config.Cloudinit`, and `VM.Config.Options` (name and tags)
  - `VM.PowerMgmt` and `VM.Audit`
  - `VM.GuestAgent.Audit` (ping) and `VM.GuestAgent.FileWrite` (JIT config). The token can't run commands in
    VMs.
  - `Pool.Audit` (read-only: without it, Proxmox hides which pool each VM is in, and the controller can't find its
    own VMs)
- on the target storage: `Datastore.AllocateSpace` and `Datastore.Audit`
- on the worker VNet (`/sdn/zones/parzone/parnet`): `SDN.Use`

`parcon install` grants exactly the list in `internal/proxmox/access.go`, and `parcon check proxmox` checks the token
against it. Keep this list in sync with that file.

To build from source, use the dev container in `.devcontainer/`, which has Go, Packer, and every other tool at
pinned versions. See [AGENTS.md](AGENTS.md#development-environment).

### Proxmox in a VM

The node can itself be a VM, for example to keep the runners apart from your main Proxmox setup. Enable nested
virtualization for it (CPU type `host`). `parcon install` and `parcon update` then turn off the offloads of the
node's virtio NIC (GRO, GSO, TSO, and TX checksumming), with a udev rule that keeps them off after a reboot: with
them on, workers' downloads stall at a few KB/s. `parcon status` shows whether they are off.

## Limitations

These are deliberate. The project supports exactly the setup `parcon install` creates, and nothing else.

- **Single standalone node only.** Proxmox clusters aren't supported, even if workers would run on only one node,
  and `parcon install` refuses to run on a cluster member. The maintainer has no hardware to test clusters.
- **One installation layout.** No container image, Kubernetes deployment, other hypervisors, or controller running
  outside its VM. The repository is public, so you are welcome to fork it for other setups.
- **One worker network.** Every scale set shares the same worker network and runner pool. Keep jobs with different
  trust levels apart with separate scale sets and GitHub runner groups.
- **Workers can reach each other.** Workers running at the same time share one network segment, so a hostile job
  can connect to other workers, answer their DHCP, or impersonate the gateway. Isolating workers through the Proxmox
  firewall is planned. Until then, run jobs that must be protected from untrusted code on a separate node.
- **amd64 only**, and workers run **Ubuntu 26.04 only**.
- **A lean runner image, not GitHub's.** Workers don't have the preinstalled toolset of GitHub-hosted runners, which
  is tens of GB. Workflows install what they need with `setup-*` actions, or you extend the Packer build in
  `images/runner/` and import your own image.
- **GitHub.com only.** GitHub Enterprise Server may work through `actions/scaleset`, but it isn't tested.
- **No metrics endpoint yet.** Metrics are deferred past v0.1. The planned endpoint uses HTTPS with a self-signed
  certificate and no authentication, and exposes no secrets.

## Repository layout (planned)

```
cmd/parcon/            `parcon`: install, status, config, update, check, and uninstall on the host; the controller
                       service and the steps that need its secrets in the controller VM
internal/config/       the controller's config: loading, defaults, validation
internal/settings/     the host's settings, the config keys, and the controller config rendered from them
internal/installer/    install, check, status, config, update, and uninstall on the host
internal/pvecli/       Proxmox through pvesh, pveum, and qm on the host
internal/hostsys/      host facts: addresses, NICs, memory, the API's certificate
internal/release/      release lookup and signature verification
internal/term/         questions on the terminal
internal/github/       scale set listener, JIT configs, GitHub App auth
internal/proxmox/      Proxmox API client (clone, configure, start, destroy, list)
internal/controller/   reconcile loop and worker lifecycle
internal/vmtags/       the Proxmox tags that hold the controller's state
install/               install.sh, the bootstrap that runs parcon install, and its bats tests
images/common/         what the image builds share: Ubuntu pin, base and cleanup steps, boot test
images/controller/     Packer build of the controller VM image with parcon (CI)
images/gateway/        Packer build of the gateway VM image: DHCP, DNS, NAT, firewall (CI)
images/runner/         Packer build of the runner template image (CI)
.github/               CI, the release build, and the daily actions/runner check
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
  Neither does the Proxmox token's secret: `parcon` pipes it into the controller VM when it creates it. The host's
  settings hold nothing secret.
- Releases are signed. `install.sh` checks `parcon` against the checksum written into it, and `parcon` accepts a
  release only if its `SHA256SUMS` carries a signature from a key built into it, then checks every file it downloads
  against those checksums.
- Metrics, once added (deferred past v0.1), will be served over HTTPS on the LAN without authentication. Don't expose
  the controller VM beyond your LAN.
- The GitHub App private key and the Proxmox token are read from files and never logged. JIT configs are
  single-use. They are written straight into the VM that uses them through the guest agent, and are never
  stored in cloud-init snippets or VM config.

## Contributing

See [AGENTS.md](AGENTS.md) for architecture invariants, conventions, and development commands. It applies to
human contributors too. Commit messages follow [Conventional Commits](https://www.conventionalcommits.org/):
[release-please](https://github.com/googleapis/release-please) turns them into each release's version and
changelog, and merging its release pull request is the only way to publish a release (AGENTS.md, *Commits and
releases*).

## License

[Apache License 2.0](LICENSE). It matches ARC and is compatible with the MIT-licensed `actions/scaleset`.
