# Installer design

*Planned.* The whole project installs with one script run as root on a Proxmox VE 9 node. The script checks the
host, creates an isolated worker network with a gateway VM, creates a small controller VM, builds the runner
template, and registers the scale set. It changes the host only through Proxmox's own tools, and it can remove
everything it created.

## Goals

- **One download, one command.** No clone, no build tools, and no packages to install first.
- **Headless.** The installer runs in a root shell with no browser. The only browser step, creating the GitHub App,
  happens on any other device, and the user copies one code back.
- **Minimal host footprint.** The host gets Proxmox objects (pools, a role, a user and token, ACLs, VMs, and an SDN
  zone and VNet) and nothing else: no apt packages, no systemd units, no binaries, no cloud-init snippets, and no
  hand edits to `/etc/network/interfaces` or `storage.cfg`.
- **Independent of the LAN.** Workers never share a network with the host or the LAN. A gateway VM connects their
  network to the internet, so the design works the same whatever the LAN, VLAN, or host firewall setup is.
- **Safe to re-run.** A second run detects the existing install and upgrades it. `--dry-run` prints the plan and
  changes nothing.
- **Reversible.** `install.sh uninstall` removes every object the installer or controller created, found by tag and
  name.

## Usage

Download, read, then run:

```bash
curl -fsSLO https://github.com/klponce/proxmox-actions-runners/releases/latest/download/install.sh
less install.sh
bash install.sh
```

Or run it in one line:

```bash
bash -c "$(curl -fsSL https://github.com/klponce/proxmox-actions-runners/releases/latest/download/install.sh)"
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
 ┌──────────────────────────────────────────────────────────────────────────────────┐
 │  pool par-system                            pool par-runners                     │
 │  ┌──────────────────────┐   PVE API +       ┌─────────────────────────────────┐  │
 │  │ controller VM        │   guest agent     │ runner template (versioned)     │  │
 │  │  config + secrets    ├──────────────────►│   │ linked clone                │  │
 │  └──────────┬───────────┘                   │   ▼                             │  │
 │             │                               │ worker VMs (one job each)       │  │
 │             │                               └───────────────┬─────────────────┘  │
 │             │                                               │                    │
 │             │                                               │ worker network:    │
 │             │                                               │ VNet parnet (no    │
 │             │                                               │ uplink, no host IP)│
 │             │           ┌──────────────────────┐            │                    │
 │             │           │ gateway VM           │ net1       │                    │
 │             │           │  DHCP, DNS, NAT,     ├────────────┘                    │
 │             │           │  firewall            │                                 │
 │             │           └──────────┬───────────┘                                 │
 │             │                      │ net0                                        │
 │  ───────────┴──────────────────────┴──── LAN bridge (e.g. vmbr0)                 │
 └─────────────────────────────┬────────────────────────────────────────────────────┘
                               ▼
                  LAN ──► Proxmox API, GitHub, internet
```

- The **controller VM** runs the controller as a systemd service and holds the config and secrets. It sits on the
  LAN because it needs the Proxmox API and GitHub. It has no NIC on the worker network and reaches workers only
  through the Proxmox API and the guest agent.
- The **gateway VM** has one NIC on the LAN and one on the worker network. It is the workers' default gateway,
  DHCP server, and DNS forwarder, and it NATs their traffic out through the LAN. Its firewall allows outbound
  internet traffic and drops traffic to the LAN, the Proxmox host, the controller VM, and private and link-local
  ranges. It holds no secrets.
- The **worker network** is an SDN simple zone `parzone` with one VNet `parnet`. It has no subnet, host IP, SNAT, or
  DHCP on the host, so the host is just a switch for it and never routes worker traffic.
- The API token can act only on the `par-runners` pool. The controller and gateway VMs live in `par-system`, so a
  compromised controller can't change or delete itself, the gateway, or any VM outside the runner pool.
- **Workers** attach only to `parnet`.

## Release assets

Each release publishes the following files. The installer contains the SHA-256 of every asset for its own version
and refuses a file that doesn't match.

| Asset | Contents | Built by |
| ----- | -------- | -------- |
| `install.sh` | The installer | Release workflow |
| `parcon-<ver>.qcow2` | Ubuntu 26.04 minimal, `qemu-guest-agent`, the controller binary and unit, nginx for the metrics endpoint | Packer `qemu` builder in CI |
| `par-gateway-<ver>.qcow2` | Ubuntu 26.04 minimal, `qemu-guest-agent`, nftables, dnsmasq, `unattended-upgrades` | Packer `qemu` builder in CI |
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
| Standalone node | `/etc/pve/corosync.conf` doesn't exist, so the node isn't in a cluster. Clusters aren't supported (see the README's *Limitations*) | hard |
| Clock synchronized | `timedatectl show -p NTPSynchronized` is `yes` (GitHub App JWTs fail when clocks drift) | hard |
| Outbound HTTPS | `github.com`, `api.github.com`, and the release asset hosts (`objects.githubusercontent.com`, `release-assets.githubusercontent.com`) | hard |
| Target storage | exists, active, and accepts `images` content | hard |
| Linked-clone support | storage type is `lvmthin`, `zfspool`, `rbd`, or file-based with qcow2. Otherwise `linkedClone: false` | warn |
| Free space | controller and gateway disks + template + `maxRunners` × (`freeDiskGiB`, or template size + `freeDiskGiB` if not linked) + image download | hard |
| Free memory and CPU | host RAM and threads against `maxRunners` × worker size plus existing VMs | warn |
| LAN bridge | the bridge for the controller and gateway VMs exists. VLAN tag valid if set | hard |
| SDN available | `ifupdown2` installed and `/etc/network/interfaces` sources `/etc/network/interfaces.d/*`, so applying SDN works | hard |
| Worker subnet free | the worker subnet doesn't overlap any route or address on the host, or the LAN subnet given for the gateway | hard |
| SDN names free | zone `parzone` and VNet `parnet` are unused, or already ours (upgrade) | hard |
| No name or ID clash | pools, user, role, and VMIDs are unused, or already tagged as ours (upgrade) | hard |
| Pending SDN changes | `pvesh get /cluster/sdn` shows no pending changes from someone else, since applying ours would apply theirs too | warn |

## Install flow

1. **Preflight.** Run the checks above.
2. **Collect settings** from flags, the answers file, or prompts: the GitHub organization or repository, scale set
   name and limits, storage, the LAN bridge and VLAN, the controller and gateway LAN IPs (DHCP or static), the
   worker subnet (default `10.251.0.0/22`, changeable if it clashes), and the VMID range.
3. **Show the plan.** List every object to be created or changed on the host, then ask for confirmation.
4. **Proxmox access objects** (`pveum`):
   - pools `par-system` and `par-runners`
   - role `PARController` with only the privileges listed in the README
   - user `par@pve` and a privilege-separated token `par@pve!controller`
   - ACLs for that token on `/pool/par-runners`, the target storage, and `/sdn/zones/parzone/parnet`
   The installer captures the token secret in a shell variable. It never writes it to the host's disk.
5. **Worker network** (`pvesh`): create the simple zone `parzone` and the VNet `parnet` with no subnet, then apply
   the SDN config (`pvesh set /cluster/sdn`).
6. **Download and verify images** into a temporary directory under `/var/tmp`, which is deleted on exit, including
   on failure.
7. **Create the runner base VM** in `par-runners` from `par-runner-base.qcow2`
   (`qm create` + `qm set --scsi0 <storage>:0,import-from=<file>`), tag it `par-managed,par-base`, and convert it to
   a template.
8. **Create the gateway VM** in `par-system` from `par-gateway.qcow2`: 1 vCPU, 1 GiB RAM, 8 GiB disk, `net0` on the
   LAN bridge and `net1` on `parnet`. Use the built-in cloud-init drive for hostname and the LAN address only, tag
   it `par-managed,par-gateway`, and start it. Once the guest agent responds, push the worker subnet and the LAN
   ranges to block through `qm guest exec --pass-stdin`, then check that it serves DHCP on `parnet` and reaches the
   internet.
9. **Create the controller VM** in `par-system` from `parcon.qcow2`: 2 vCPU, 2 GiB RAM, 20 GiB disk. Use
   Proxmox's built-in cloud-init drive for hostname and network only (no user data, no snippets), tag it
   `par-managed,par-controller`, and start it.
10. **Configure the controller** through the guest agent once it responds. Secrets go through
    `qm guest exec --pass-stdin`, so they never appear on a command line or on the host's disk:
    - `/etc/proxmox-actions-runners/config.yaml` with the settings and the Proxmox host's pinned TLS fingerprint
    - the Proxmox token as a file readable only by the controller's user
    The installer then runs `parcon check proxmox` in the VM. It confirms the Proxmox VE version, that the token has
    every privilege it needs on the pool, storage, and VNet, and that the storage accepts VM disks.
11. **Create the GitHub App** with the manifest flow described under *GitHub App setup*. The code the user pastes is
    passed to `parcon github app create` in the controller VM, so the App's private key goes straight from GitHub
    into the controller VM and never passes through the host. A re-run skips this step if the controller VM already
    holds working App credentials. The installer then runs `parcon check github` in the VM. This confirms that the
    App credentials produce an installation token and can reach the org or repo, and it catches bad credentials
    before the long template build. The service is enabled and started after that.
12. **Build the runner template.** The installer runs `parcon template build` in the controller VM and streams its
    progress. This is the long step: installing the full toolset takes roughly an hour depending on bandwidth.
    `--template minimal` skips the full toolset for a fast first install.
13. **Smoke test.** The controller clones one worker, confirms the guest agent responds, and confirms through
    `guest-exec` that the worker got a DHCP lease, reaches GitHub, and can't reach the Proxmox API or the controller
    VM. It then destroys the clone, registers the scale set with GitHub, and confirms the listener session.
14. **Summary.** Print the `runs-on:` label, the controller and gateway VMs' IDs and IPs, the metrics URL and its
    certificate's SHA-256 fingerprint, and the upgrade and uninstall commands.

If any step fails, the installer stops and prints what it already created, so a re-run can continue from there.
Each step checks for existing objects before it creates anything.

## GitHub App setup

GitHub Apps can only be created in a browser: there is no API for it, and the OAuth device flow (where you type a
code shown by a CLI) only works for an App that already exists. The installer therefore uses GitHub's
[manifest flow](https://docs.github.com/en/apps/sharing-github-apps/registering-a-github-app-from-a-manifest) with
a static helper page, and the user copies one code back:

1. The installer prints a URL to the helper page with the target and a random `state` in the query string, for
   example `https://klponce.github.io/proxmox-actions-runners/app/v1/?org=my-org&state=…`. The user opens it on any
   device with a browser.
2. The page builds the manifest and submits it as a form `POST` to
   `https://github.com/organizations/<org>/settings/apps/new`, or `https://github.com/settings/apps/new` for a
   repository owned by a personal account. The manifest sets:
   - a name, `par-<owner>-<random>`, since App names are unique across GitHub
   - the permission the scope needs: **Self-hosted runners: write** for an organization, or
     **Administration: write** for a repository
   - an inactive webhook (`hook_attributes.active: false`), because the controller long-polls
   - `redirect_url` back to the same page, and `public: false`
3. The user reviews the App on GitHub and clicks **Create**. GitHub redirects back to the page with `code` and
   `state`. The page shows them as one string to copy.
4. The user pastes the string into the installer. The installer checks `state`, then passes the code to
   `parcon github app create` in the controller VM, which calls `POST /app-manifests/{code}/conversions` and stores
   the returned Client ID and private key. The code is single-use and expires after one hour.
5. The installer prints the App's install link (`https://github.com/apps/<slug>/installations/new`). The user
   installs the App on the organization or repository, and `parcon` polls `GET /app/installations` until the
   installation appears, then stores its ID. No second copy-back is needed.

About the helper page:

- It lives in `site/` and is published with GitHub Pages. It is plain HTML and JavaScript with no third-party
  scripts, analytics, or cookies, and it sends nothing anywhere except the form `POST` to GitHub.
- Its path is versioned (`/app/v1/`). A change to the manifest gets a new path, so older installers keep working.
- The code on the page can be exchanged for the App's private key by anyone who has it, until it is used or expires.
  The page says so, and the installer exchanges it at once. Neither the installer nor `parcon` logs it.
- For users who would rather not use the page, `--github-app manual` asks for an existing App's Client ID and reads
  its private key from standard input, passing it straight to the controller VM.

Typing a code from the installer into the page (the device-flow style) isn't possible: the page is static, so it
has no way to send the result back to the installer. That would need a hosted relay service, which would also see
the code that unlocks the private key.

## Metrics endpoint

`parcon` serves `/metrics`, `/healthz`, and `/readyz` as plain HTTP on `127.0.0.1:9465` only. nginx (from Ubuntu
main, so `unattended-upgrades` patches it) runs in the controller VM and serves them over HTTPS on the LAN at
`https://<controller-ip>:9464/`.

- The certificate is self-signed (ECDSA P-256) with the VM's hostname and LAN addresses as subject alternative
  names. It is generated inside the controller VM on first boot, never baked into the image, since every install
  would then share the same key. A timer regenerates it 30 days before it expires, which is 2 years after it was
  issued.
- The installer prints the certificate's SHA-256 fingerprint so Prometheus can be pointed at it with `ca_file`, or
  at least checked by hand.
- There is no authentication for now. The endpoint exposes counts, latencies, and scale set names, but no secrets.
  Workers can't reach it because the gateway blocks the LAN.
- `/readyz` fails while the GitHub message session or the Proxmox API is unreachable. `/healthz` only checks that the
  process is running.

## Template build

`parcon template build` runs inside the controller VM and uses only the Proxmox API:

1. Linked-clone the base template into a build VM on the worker network, tagged `par-managed,par-build`, and grow
   its disk to fit the toolset plus a small margin.
2. Boot it, then push the provisioning scripts from `images/ubuntu-26.04/` (shipped in the controller image) through
   the guest agent and run them with `guest-exec`. This needs no SSH and no network path from the controller to the
   build VM.
3. Clean up inside the guest: reset the machine-id, SSH host keys, and cloud-init state, and clear logs.
4. Shut down, convert to a template named with its version, and tag it `par-managed,par-template,<version>`. The
   template's disk size is the baseline that each worker's `freeDiskGiB` is added to, so keep it tight.
5. Smoke-test one clone, then point new workers at the new template.

Templates are immutable. Linked clones depend on their template, so an old template is deleted only once no worker
uses it. The controller repeats this build weekly and whenever a new `actions/runner` version is released.

## Networking

There is one networking mode. Workers sit on a dedicated network, and the gateway VM is the only path between it and
the LAN:

| Piece | Where | Role |
| ----- | ----- | ---- |
| SDN zone `parzone`, VNet `parnet` | Proxmox host | An isolated layer-2 segment. No subnet, host IP, SNAT, or DHCP on the host |
| Gateway VM `net1` | `parnet` | Workers' default gateway, DHCP server, and DNS forwarder (dnsmasq) for the worker subnet |
| Gateway VM `net0` | LAN bridge | NATs worker traffic to the LAN's router, like any other LAN client |
| Gateway firewall | Gateway VM (nftables) | Allows DHCP, DNS, and new outbound connections to the internet. Drops the LAN, the Proxmox host, the controller VM, RFC 1918, CGNAT, and link-local ranges, and any traffic to the gateway itself except DHCP and DNS |

Workers use DHCP (`ipconfig0: ip=dhcp` on the built-in cloud-init drive), so the controller keeps no address
state. The gateway's leases are short, sized so the subnet can't run out at `maxRunners`.

Why a gateway VM rather than SNAT on the host (an SDN zone with SNAT and the host's own DHCP) or attaching workers to
an existing bridge or VLAN:

- The host never routes untrusted traffic, so worker isolation doesn't depend on the host's forwarding, firewall, or
  `pve-firewall` setup, which varies from host to host.
- SDN DHCP on the host needs the `dnsmasq` package, which invariant 8 rules out.
- Attaching workers to an existing bridge makes isolation depend on the user's switch, router, and VLAN setup, which
  the installer can't check.

The gateway is a single point of failure for worker networking. The controller's metrics show it as jobs that
stay queued and workers that never register, and `install.sh check` tests it. Its OS updates itself with
`unattended-upgrades`, and `install.sh upgrade` replaces it with a new image.

## Upgrade and uninstall

- **Upgrade** is in place: the new controller binary is pushed through the guest agent, then the service is
  restarted. Config and secrets stay in the controller VM. The gateway VM holds no state beyond its settings, so it
  is replaced with a new image and reconfigured. The runner template is rebuilt with the new
  provisioning scripts. The controller VM's OS updates itself with `unattended-upgrades`.
- **Uninstall** first stops the controller and deletes the scale set in GitHub. It then destroys every VM tagged
  `par-managed`, removes the ACLs, token, user, role, and pools, and removes the `parzone` zone and `parnet` VNet and
  applies the SDN config. It doesn't touch anything it didn't create.

## Script conventions

- Bash with `set -euo pipefail`. The whole body is inside `main` and called on the last line, so a partial
  `curl | bash` download can't run half a script.
- Use only tools that ship with Proxmox VE 9. Parse JSON with `pvesh --output-format json` and `perl -MJSON`, not
  `jq`.
- Every mutating command goes through one `run` function that honors `--dry-run` and logs the command with
  secrets masked.
- Must pass `shellcheck`. Tests use `bats` with stubbed `qm`, `pveum`, and `pvesh`.
