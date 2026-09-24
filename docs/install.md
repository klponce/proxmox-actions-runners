# Installer design

*Planned.* The whole project installs with one script run as root on a Proxmox VE 9 node. The script checks the
host, creates a small controller VM, builds the runner template, and registers the scale set. It changes the host
only through Proxmox's own tools, and it can remove everything it created.

## Goals

- **One download, one command.** No clone, no build tools, and no packages to install first.
- **Minimal host footprint.** The host gets Proxmox objects (pools, a role, a user and token, ACLs, VMs) and
  nothing else: no apt packages, no systemd units, no binaries, no cloud-init snippets, and no edits to
  `/etc/network/interfaces` or `storage.cfg`. The only exception is SDN networking, and only if you opt in.
- **Safe to re-run.** A second run detects the existing install and upgrades it. `--dry-run` prints the plan and
  changes nothing.
- **Reversible.** `install.sh uninstall` removes every object the installer or controller created, found by tag and
  name.

## Usage

Download, read, then run:

```bash
curl -fsSLO https://github.com/<owner>/proxmox-actions-runners/releases/latest/download/install.sh
less install.sh
bash install.sh
```

Or run it in one line:

```bash
bash -c "$(curl -fsSL https://github.com/<owner>/proxmox-actions-runners/releases/latest/download/install.sh)"
```

Without flags, the script prompts for each setting. For unattended installs, pass an answers file:

```bash
bash install.sh --answers par.env --yes
```

| Command or flag | Effect |
| --------------- | ------ |
| `install` (default) | Install, or upgrade if an install is found |
| `upgrade` | Upgrade the controller and rebuild the template. Keeps config and secrets |
| `uninstall` | Remove all managed VMs, templates, the controller VM, and the Proxmox objects |
| `check` | Run the preflight checks only |
| `--dry-run` | Print every change it would make, then exit |
| `--answers <file>` | Read settings from a `KEY=value` file instead of prompting |
| `--yes` | Skip the confirmation prompt |
| `--version <tag>` | Install a specific release instead of the one the script belongs to |

## Architecture

```
 Proxmox VE 9 node (host: only Proxmox objects are added)
 ┌───────────────────────────────────────────────────────────────────────────┐
 │  pool par-system                    pool par-runners                      │
 │  ┌──────────────────────┐           ┌─────────────────────────────────┐   │
 │  │ controller VM        │  PVE API  │ runner template (versioned)     │   │
 │  │  proxmox-actions-    ├──────────►│   │ linked clone                │   │
 │  │  runners controller  │           │   ▼                             │   │
 │  │  config + secrets    │           │ worker VMs (one job each)       │   │
 │  └──────────┬───────────┘           └───────────────┬─────────────────┘   │
 │   management bridge                       runner bridge / VLAN            │
 └─────────────┼───────────────────────────────────────┼─────────────────────┘
               ▼                                       ▼
       Proxmox API + GitHub                    internet egress only
```

- The **controller VM** runs the controller as a systemd service and holds the config and secrets. It sits on the
  management network because it needs the Proxmox API.
- The API token can act only on the `par-runners` pool. The controller VM lives in `par-system`, so a compromised
  controller can't change or delete itself or any VM outside the runner pool.
- **Workers** attach only to the runner bridge or VLAN.

## Release assets

Each release publishes the following files. The installer contains the SHA-256 of every asset for its own version
and refuses a file that doesn't match.

| Asset | Contents | Built by |
| ----- | -------- | -------- |
| `install.sh` | The installer | Release workflow |
| `parcon-<ver>.qcow2` | Ubuntu 26.04 minimal, `qemu-guest-agent`, the controller binary and unit | Packer `qemu` builder in CI |
| `par-runner-base-<ver>.qcow2` | Ubuntu 26.04 cloud image, `qemu-guest-agent`, `runner` user, no toolset | Packer `qemu` builder in CI |
| `SHA256SUMS` | Checksums of the above | Release workflow |

The full toolset for parity with GitHub-hosted runners is too large to ship as a download (tens of GB). The
controller installs it locally on top of the runner base image (see *Template build*).

**Why prebuilt images instead of stock Ubuntu cloud images:** stock images don't include `qemu-guest-agent`, and the
only way to add it at first boot is custom cloud-init user data. Proxmox stores that as snippet files, which means
enabling the `snippets` content type on a host storage and writing files to the host. Shipping images that already
include the agent avoids both changes to the host.

## Preflight checks

`install.sh check` runs these, and `install` runs them first. A failed **hard** check stops the install. A failed
**warn** check is shown in the plan and needs confirmation (or `--yes`).

| Check | How | Level |
| ----- | --- | ----- |
| Running as root | `EUID` is 0 (`pveum` and `qm` need `root@pam`) | hard |
| Proxmox VE 9.x | `pveversion` reports `pve-manager/9.*` | hard |
| Running on a PVE node, not in a container or VM guest | `pveversion` present, `/etc/pve` mounted, `systemd-detect-virt --container` false | hard |
| amd64 | `dpkg --print-architecture` is `amd64` | hard |
| KVM available | `/dev/kvm` exists | hard |
| Required tools present | `qm`, `pveum`, `pvesh`, `pvesm`, `curl`, `sha256sum`, `perl` (all ship with PVE) | hard |
| Cluster quorate | `pvecm status` if clustered | hard |
| Clock synchronized | `timedatectl show -p NTPSynchronized` is `yes` (GitHub App JWTs fail when clocks drift) | hard |
| Outbound HTTPS | `github.com`, `api.github.com`, and the release asset hosts (`objects.githubusercontent.com`, `release-assets.githubusercontent.com`) | hard |
| Target storage | exists, active, and accepts `images` content | hard |
| Linked-clone support | storage type is `lvmthin`, `zfspool`, `rbd`, or file-based with qcow2. Otherwise `linkedClone: false` | warn |
| Free space | controller disk + template + `maxRunners` × worker disk (full size if not linked) + image download | hard |
| Free memory and CPU | host RAM and threads against `maxRunners` × worker size plus existing VMs | warn |
| Bridges | the management and runner bridges exist. VLAN tag valid if set | hard |
| No name or ID clash | pools, user, role, and VMIDs are unused, or already tagged as ours (upgrade) | hard |
| Runner network separated | the runner bridge differs from the management bridge or carries a VLAN tag. Workers sharing the management network can reach the Proxmox API | warn |

## Install flow

1. **Preflight.** Run the checks above.
2. **Collect settings** from flags, the answers file, or prompts: GitHub target URL and App credentials, scale set
   name and limits, storage, management bridge and controller IP (DHCP or static), runner bridge and VLAN, and the
   VMID range.
3. **Show the plan.** List every object to be created or changed on the host, then ask for confirmation.
4. **Proxmox access objects** (`pveum`):
   - pools `par-system` and `par-runners`
   - role `PARController` with only the privileges listed in the README
   - user `par@pve` and a privilege-separated token `par@pve!controller`
   - ACLs for that token on `/pool/par-runners`, the target storage, and the runner bridge's SDN zone
   The installer captures the token secret in a shell variable. It never writes it to the host's disk.
5. **Download and verify images** into a temporary directory under `/var/tmp`, which is deleted on exit, including
   on failure.
6. **Create the runner base VM** in `par-runners` from `par-runner-base.qcow2`
   (`qm create` + `qm set --scsi0 <storage>:0,import-from=<file>`), tag it `par-managed,par-base`, and convert it to
   a template.
7. **Create the controller VM** in `par-system` from `parcon.qcow2`: 2 vCPU, 2 GiB RAM, 20 GiB disk. Use
   Proxmox's built-in cloud-init drive for hostname and network only (no user data, no snippets), tag it
   `par-managed,par-controller`, and start it.
8. **Configure the controller** through the guest agent once it responds. Secrets go through
   `qm guest exec --pass-stdin`, so they never appear on a command line or on the host's disk:
   - `/etc/proxmox-actions-runners/config.yaml` with the settings and the Proxmox host's pinned TLS fingerprint
   - the Proxmox token and GitHub App key as files readable only by the controller's user
   The installer then runs `parcon check github` in the VM. This confirms that the App credentials produce an
   installation token and can reach the org or repo, and it catches bad credentials before the long template build.
   The service is enabled and started after that.
9. **Build the runner template.** The installer runs `parcon template build` in the controller VM and streams its
   progress. This is the long step: installing the full toolset takes roughly an hour depending on bandwidth.
   `--template minimal` skips the full toolset for a fast first install.
10. **Smoke test.** The controller clones one worker, confirms the guest agent responds, and destroys the clone.
    Then it registers the scale set with GitHub and confirms the listener session.
11. **Summary.** Print the `runs-on:` label, the controller VM's ID and IP, and the upgrade and uninstall commands.

If any step fails, the installer stops and prints what it already created, so a re-run can continue from there.
Each step checks for existing objects before it creates anything.

## Template build

`parcon template build` runs inside the controller VM and uses only the Proxmox API:

1. Linked-clone the base template into a build VM on the runner network, tagged `par-managed,par-build`.
2. Boot it, then push the provisioning scripts from `images/ubuntu-26.04/` (shipped in the controller image) through
   the guest agent and run them with `guest-exec`. This needs no SSH and no network path from the controller to the
   build VM.
3. Clean up inside the guest: reset the machine-id, SSH host keys, and cloud-init state, and clear logs.
4. Shut down, convert to a template named with its version, and tag it `par-managed,par-template,<version>`.
5. Smoke-test one clone, then point new workers at the new template.

Templates are immutable. Linked clones depend on their template, so an old template is deleted only once no worker
uses it. The controller repeats this build weekly and whenever a new `actions/runner` version is released.

## Networking

| Mode | Host changes | Isolation |
| ---- | ------------ | --------- |
| `bridge` (default) | None. Workers attach to an existing bridge, with a VLAN tag if one is given | Up to your switch, router, or VLAN setup. The installer warns if it can't confirm isolation |
| `sdn` (opt-in) | Creates an SDN simple zone and VNet with SNAT, plus Proxmox firewall rules that block the host and management network from the VNet | Enforced on the host |

The `sdn` mode is the one place the installer changes host networking, so it is opt-in and uninstall removes it.
Workers get addresses from the controller's own IPAM through cloud-init `ipconfig`, so the host needs no DHCP
server (`dnsmasq`).

## Upgrade and uninstall

- **Upgrade** is in place: the new controller binary is pushed through the guest agent, then the service is
  restarted. Config and secrets stay in the controller VM. The runner template is rebuilt with the new
  provisioning scripts. The controller VM's OS updates itself with `unattended-upgrades`.
- **Uninstall** first stops the controller and deletes the scale set in GitHub. It then destroys every VM tagged
  `par-managed`, removes the ACLs, token, user, role, and pools, and removes the SDN objects if it created them. It
  doesn't touch anything it didn't create.

## Script conventions

- Bash with `set -euo pipefail`. The whole body is inside `main` and called on the last line, so a partial
  `curl | bash` download can't run half a script.
- Use only tools that ship with Proxmox VE 9. Parse JSON with `pvesh --output-format json` and `perl -MJSON`, not
  `jq`.
- Every mutating command goes through one `run` function that honors `--dry-run` and logs the command with
  secrets masked.
- Must pass `shellcheck`. Tests use `bats` with stubbed `qm`, `pveum`, and `pvesh`.

## Open questions

- How are GitHub Apps created? A manifest flow (open a URL, click create) would avoid copying three values by hand,
  but it needs a browser and a callback.
- Multi-node clusters: workers on other nodes need the template on shared storage. Version 1 targets a single node.
- Should the controller VM image also be offered as an OCI image for people who would rather run it on Kubernetes?
