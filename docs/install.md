# Install and host commands

`parcon`, run as root on a standalone Proxmox VE 9 node, installs and manages the whole project. `parcon install`
checks the host, creates an isolated worker network with a gateway VM, creates a small controller VM, imports the
runner template, and registers the scale set. After that, `parcon status`, `parcon config`, `parcon update`, and
`parcon uninstall` are how the user runs it; the gateway and controller VMs need no logins, since `parcon` reaches
into them through the guest agent. [`install/install.sh`](../install/install.sh) is only the bootstrap: it
downloads the release's `parcon`, checks it against the checksum written into it, and runs `parcon install`.

`parcon` changes the host only where the project needs it, almost entirely through Proxmox's own tools, and it can
remove everything it created. It installs only from a signed release.

## Goals

- **One download, one command.** No clone, no build tools, and no packages to install first.
- **Headless.** `parcon` runs in a root shell with no browser. The only browser step, creating the GitHub App,
  happens on any other device, and the user copies one code back.
- **Minimal host footprint.** The host gets `parcon` itself, its settings, Proxmox objects (pools, a role, a user
  and token, ACLs, VMs, and an SDN zone and VNet), and host tuning only where the runners need it: on a node that is
  itself a VM, a udev rule that turns off the LAN NIC's offloads (see *Host NIC offloads*). No apt packages, no
  services, no cloud-init snippets, and no edits to files Proxmox or the user manages, such as
  `/etc/network/interfaces` or `storage.cfg`.
- **Independent of the LAN.** Workers never share a network with the host or the LAN. A gateway VM connects their
  network to the internet, so the design works the same whatever the LAN, VLAN, or host firewall setup is.
- **No-touch VMs.** Everything the user does, they do with `parcon` on the host. The controller VM keeps the secrets,
  which never leave it; the host keeps the settings, which hold none.
- **Safe to re-run.** A second `parcon install` continues an install that stopped, and `parcon update` continues an
  update that stopped. `--dry-run` prints the plan and changes nothing.
- **Signed updates.** `parcon update` installs only a release whose checksums carry a signature from a key built
  into `parcon`.
- **Reversible.** `parcon uninstall` removes every object `parcon` or the controller created, found by tag and name,
  and `parcon` itself.

## Usage

Download, read, then run the bootstrap:

```bash
curl -fsSLO https://github.com/klponce/proxmox-actions-runners/releases/latest/download/install.sh
less install.sh
bash install.sh
```

Or run it in one line:

```bash
bash -c "$(curl -fsSL https://github.com/klponce/proxmox-actions-runners/releases/latest/download/install.sh)"
```

`install.sh` passes its arguments to `parcon install`. Every setting has a default, so `parcon` asks only for
confirmation and for the GitHub App step (see *GitHub App setup*), where you choose the organization or repository on
GitHub. `parcon install -h` lists the settings:

```bash
bash install.sh --storage local-zfs --set runners.max=4 --set worker.memory=16GiB
```

| Flag | Default | Setting |
| ---- | ------- | ------- |
| `--bridge` | `vmbr0` | LAN bridge for the controller and gateway VMs |
| `--vlan` | none | VLAN tag on the LAN bridge |
| `--pve-address` | the host's address on the bridge | the address the controller reaches the API on |
| `--gateway-ip`, `--controller-ip` | `dhcp` | the VMs' LAN addresses: `dhcp` or a CIDR such as `192.0.2.10/24` |
| `--lan-gateway` | none | the LAN's router, needed with a static address |
| `--worker-subnet` | `10.251.0.0/22` | the worker network |
| `--storage` | `local-lvm` | storage for VM disks |
| `--vmid-range` | `10000-10099` | VMIDs for workers and templates |
| `--scale-set` | `proxmox-ubuntu-26.04` | the scale set's name, used in `runs-on` |
| `--labels` | the scale set's name | comma-separated `runs-on` labels |
| `--runner-group` | `default` | the GitHub runner group; repositories must use `default` |
| `--min-runners` | `0` | idle workers to keep booted |
| `--github-url` | where you install the App | `https://github.com/<org>` or `https://github.com/<owner>/<repo>` |
| `--set KEY=VALUE` | | any config key (see *Host settings*), repeatable |
| `--app manual --client-id ID` | `--app manifest` | use an existing GitHub App instead of creating one |
| `--dry-run`, `--yes` | | print the plan and stop; answer yes to every question |

The settings are saved on the host when the install starts, so a re-run continues with them and refuses new ones.
Each release's `install.sh` installs exactly its own release; on a node with an older install, it updates it.

After the install, `parcon` in `/usr/local/bin` does the rest:

| Command | Effect |
| ------- | ------ |
| `parcon status [--json]` | The state of the whole install; exits 1 if anything failed |
| `parcon config get <key>`, `get --all` | A setting's value, or every setting with its default and what it is |
| `parcon config set <key> <value>` | Change a setting and apply it: the controller restarts with it |
| `parcon config describe [<key>]` | Which values a setting takes, and when a change applies |
| `parcon config apply` | Push the settings to the controller again, for example after the API's pinned certificate was renewed |
| `parcon update [--pre] [--yes] [--dry-run]` | Update to the newest release, or pre-release with `--pre` |
| `parcon check` | Every check below, and after an install, the controller's own checks |
| `parcon check network` | The worker network check from step 14, on demand |
| `parcon check config\|proxmox\|github\|template` | One of the controller's checks, run in the controller VM |
| `parcon uninstall [--yes] [--dry-run]` | Remove everything, `parcon` included |

## Architecture

```
 Proxmox VE 9 node (host: parcon, its settings, and Proxmox objects)
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
- **`parcon` on the host** drives Proxmox through `pvesh`, `pveum`, and `qm` as root, and the two VMs through
  `qm guest exec`. It holds the settings but no secrets: the token's secret and the App's key go straight into the
  controller VM on a command's stdin.

## Release assets

Each release publishes the following files, built by `.github/workflows/release.yml` when release-please's release
pull request is merged (see *Commits and releases* in [AGENTS.md](../AGENTS.md)). Versions follow semver, chosen by
release-please from the Conventional Commits since the last release. Each image is boot-tested before it is
published, and the release stays a draft until every file is uploaded.

| Asset | Contents | Built by |
| ----- | -------- | -------- |
| `install.sh` | The bootstrap, with this release's version and `parcon`'s checksum written in | Release workflow |
| `parcon-<ver>-linux-amd64` | `parcon`, for the host and the controller VM | Release workflow |
| `parcon-<ver>.qcow2` | The controller VM: Ubuntu 26.04, `qemu-guest-agent`, `unattended-upgrades`, the `parcon` binary, user, and unit | Packer `qemu` builder in CI (`images/controller/`) |
| `par-gateway-<ver>.qcow2` | The gateway VM: Ubuntu 26.04, `qemu-guest-agent`, `unattended-upgrades`, nftables, dnsmasq, `par-gateway-configure` | Packer `qemu` builder in CI (`images/gateway/`) |
| `par-runner-<ver>.qcow2` | The runner template: Ubuntu 26.04, `qemu-guest-agent`, the `runner` user, a pinned `actions/runner`, Docker, and a few basics | Packer `qemu` builder in CI (`images/runner/`) |
| `par-runner-<ver>.json` | The runner image's build manifest, including its `actions/runner` version | Packer `qemu` builder in CI |
| `SHA256SUMS` | Checksums of all of the above | Release workflow |
| `SHA256SUMS.sig` | An Ed25519 signature of `SHA256SUMS` by the release signing key | Release workflow |

The chain of trust:

1. `install.sh` holds the version and `parcon`'s SHA-256 (its `@PAR_VERSION@` and `@PAR_PARCON_SHA256@`
   placeholders, filled by `install/fill-release.sh`), and runs a downloaded `parcon` only if it matches. A copy from
   the source tree refuses to run.
2. `parcon` carries the public release signing keys (`internal/release/keys`). It accepts a release only if one of
   them verifies its `SHA256SUMS.sig`, then checks every asset it downloads against `SHA256SUMS`. `parcon update`
   does the same from the running `parcon`, so an update needs neither a new `install.sh` nor trust in anything but
   the key.
3. The release workflow signs with a key only its `publish` job can read (the `release` environment admits only
   `main`), and checks the signature against those public keys before it publishes (see *Release signing* in
   [AGENTS.md](../AGENTS.md)).

A version with a suffix, such as `0.2.0-rc.1`, is published as a pre-release: its `install.sh` installs it and
`parcon update --pre` updates to it, but the `releases/latest` links in *Usage* and a plain `parcon update` keep to
the latest full release.

**Why prebuilt images instead of stock Ubuntu cloud images:** stock images don't include `qemu-guest-agent`, and the
only way to add it at first boot is custom cloud-init user data. Proxmox stores that as snippet files, which means
enabling the `snippets` content type on a host storage and writing files to the host. Shipping images that already
include the agent avoids both changes to the host.

## Preflight checks

`parcon install` runs these first, and `parcon check` runs them any time. A failed **hard** check stops the install. A
failed **warn** check is shown in the plan and needs confirmation (or `--yes`). Before an install, `parcon check`
checks the default settings; `parcon install --dry-run` with your flags checks yours. After an install, it checks the
saved settings, skips the free space an install needs, and also runs the controller's checks in its VM.

| Check | How | Level |
| ----- | --- | ----- |
| Running as root | the effective user ID is 0 (`pveum` and `qm` need `root@pam`) | hard |
| Proxmox VE 9.x | `pveversion` reports `pve-manager/9.*` | hard |
| Running on a PVE node, not in a container | `pveversion` present, `/etc/pve` mounted, `systemd-detect-virt --container` false. A node that is itself a VM is fine | hard |
| KVM available | `/dev/kvm` exists | hard |
| Required tools present | `qm`, `pveum`, `pvesh`, `pvesm`, and `ip` (all ship with PVE). The bootstrap checks for amd64, the only build of `parcon` | hard |
| Standalone node | `/etc/pve/corosync.conf` doesn't exist, so the node isn't in a cluster. Clusters aren't supported (see the README's *Limitations*) | hard |
| Clock synchronized | `timedatectl show -p NTPSynchronized` is `yes` (GitHub App JWTs fail when clocks drift) | hard |
| Outbound HTTPS | `github.com`, `api.github.com`, and the release asset hosts (`objects.githubusercontent.com`, `release-assets.githubusercontent.com`) | hard |
| Target storage | exists, active, and accepts `images` content | hard |
| Linked-clone support | storage type is `lvmthin`, `zfspool`, `rbd`, or file-based with qcow2. Otherwise `linkedClone: false` | warn |
| Free space | 50 GiB free on the VM storage: the gateway, controller, and template disks, and room for one worker | hard |
| Download space | 6 GiB free in `/var/tmp`, on the host's root filesystem, where the images are downloaded before they are imported | hard |
| Free memory and CPU | host RAM and threads against `maxRunners` × worker size plus existing VMs | warn |
| LAN bridge | the bridge for the controller and gateway VMs exists. VLAN tag valid if set | hard |
| LAN NIC offloads | on a node that is itself a VM (a virtio NIC under the LAN bridge), its offloads are off, now and at boot. `parcon install` and `parcon update` turn them off; `parcon check` warns until they are | warn |
| API certificate | how the controller will verify it: the node's CA, the system CAs, or a pinned fingerprint for a certificate from a CA the host doesn't trust | warn if pinned |
| SDN available | `ifupdown2` installed and `/etc/network/interfaces` sources `/etc/network/interfaces.d/*`, so applying SDN works | hard |
| Worker subnet free | the worker subnet doesn't overlap any route or address on the host, or the LAN subnet given for the gateway | hard |
| SDN names free | zone `parzone` and VNet `parnet` are unused, or ours from an earlier run | hard |
| No name or ID clash | pools, user, role, and VMIDs are unused, or ours from an earlier run | hard |
| Pending SDN changes | no SDN zone or VNet other than ours has pending changes, since applying ours would apply theirs too. Uninstall doesn't apply while any exist, and says so | hard |

## Controller VM contract

The controller image, `parcon` on the host, and the controller agree on this layout:

- **User:** a system user `parcon` with no login shell, created by the controller image. `parcon run` runs as
  `parcon` from a systemd unit.
- **Binary:** `/usr/local/bin/parcon`, baked into the controller image. `parcon update` has the VM download the new
  release's binary and check it against the verified `SHA256SUMS`, replaces the file, and restarts the unit.
- **Config and secrets:** the directory `/etc/proxmox-actions-runners` is owned by `parcon:parcon` with mode `0700`.
  Every file in it is owned by `parcon` with mode `0600`: `config.yaml`, `pve-token`, `github-app.pem`, and, when
  the API serves the node's own certificate, `pve-ca.pem`.
- **Commands:** `parcon` on the host runs every `parcon` command in the VM as `parcon`, with
  `runuser -u parcon -- parcon …` through `qm guest exec`, so files that `parcon` writes get the right owner.
- **Before the App exists:** `github.app` (`clientId`, `installationId`, `privateKeyFile`) may be left out of
  `config.yaml`, because step 12 checks Proxmox before step 13 creates the App. `parcon check config`,
  `parcon check proxmox`, and `parcon check template` work without it. `parcon run` and `parcon check github` fail
  with "the GitHub App isn't set up yet" until all three fields are set.
- **Who writes the config:** only `parcon` on the host, which renders it from the host settings (see *Host
  settings*) and rewrites it whenever they change. It writes `config.yaml.new`, has the VM's own
  `parcon check config -config config.yaml.new` accept it, and only then moves it into place, so a config that
  version can't read never replaces a working one. In the VM, `parcon` prints the non-secret values it learns (Client
  ID, App ID, slug, installation ID) and writes secrets only to their own files; it never writes YAML.
- **Status:** the running controller writes a non-secret report to `/run/parcon/status.json` after each pass
  (`RuntimeDirectory=parcon` in the unit): each scale set's session, desired and actual workers, and GitHub's last
  job counts. `parcon status` reads it through the guest agent. The controller never reads it back.

## Host settings

`/etc/proxmox-actions-runners/settings.yaml` on the host (root, `0600`) is the source of truth for the install:

| Section | What | Written by |
| ------- | ---- | ---------- |
| `network` | the bridge, VLAN, API address, the VMs' LAN addresses and router, the worker subnet | install flags |
| `proxmox` | the storage and the VMID range | install flags |
| `github` | the organization or repository, and the App's Client ID and installation ID | the install, as it learns them |
| `scaleSet` | the name, labels, runner group, and the most and fewest workers | install flags, `runners.max` |
| `worker` | worker hardware; a field left out uses the GitHub-matching default | `worker.cores`, `worker.memory` |

It holds nothing secret. `parcon` renders the controller's `config.yaml` from it together with what it reads from
the node each time (the node's name, the API's address and certificate, and whether the storage can make linked
clones), and checks the result with the controller's own config validation before it pushes it. The defaults live
only in `internal/config`, like every other default.

The **config keys** are the settings `parcon config` changes. Each has a description, the values it takes on this
node (for example, no more vCPUs than the node has), a default, and says when a change applies:

| Key | Values | Default |
| --- | ------ | ------- |
| `runners.max` | 1 to the number of worker IDs in the VMID range, and at least `minRunners` | 1 |
| `worker.cores` | 1 to the node's CPU threads | 2 |
| `worker.memory` | 1GiB to the node's memory, in GiB or MiB, such as `8GiB` or `12288MiB` | 8GiB |

`parcon config set` refuses a value outside them with the key's description, the same text `parcon config describe`
prints, and exits 2. It warns, but allows, a combination the node likely can't run, such as more worker memory in
total than the node has. For a value it accepts, it:

1. refuses if the controller runs a different `parcon` than the host (run `parcon update` first);
2. pushes the new config the safe way described in the *Controller VM contract*;
3. saves the settings;
4. restarts `parcon.service` and waits for its scale set session. Running workers keep their jobs: a restarted
   controller adopts them. New workers get the new hardware.

Adding a key means adding it to the registry in `internal/settings/keys.go`.

## Install flow

1. **Preflight.** Run the checks above.
2. **Collect settings** from the flags, or the defaults: scale set name and limits, storage, the LAN bridge and VLAN,
   the controller and gateway LAN IPs (DHCP or static), the worker subnet (default `10.251.0.0/22`, changeable if it
   clashes), and the VMID range. An earlier run's saved settings take their place.
3. **Show the plan.** List every object to be created or changed on the host, then ask for confirmation.
4. **`parcon` on the host:** copy the running `parcon` to `/usr/local/bin/parcon` and save the settings.
5. **Proxmox access objects** (`pveum`):
   - pools `par-system` and `par-runners`
   - role `PARController` with only the privileges in `internal/proxmox/access.go`, listed in the README
   - user `par@pve` and a privilege-separated token `par@pve!controller`
   - ACLs for that token on `/pool/par-runners`, the target storage, and `/sdn/zones/parzone/parnet`
   `parcon` holds the token secret only in memory and pipes it into the controller VM. It never reaches the host's
   disk, a command line, or the output.
6. **Worker network** (`pvesh`): create the simple zone `parzone` and the VNet `parnet` with no subnet, then apply
   the SDN config (`pvesh set /cluster/sdn`).
7. **Host NIC offloads**, only on a node that is itself a VM: see *Host NIC offloads*.
8. **Verify the release**: download `SHA256SUMS` and `SHA256SUMS.sig` and check the signature. Each image is then
   downloaded when a step needs it, into a temporary directory under `/var/tmp` that is removed afterward, and checked
   against `SHA256SUMS`.
9. **Import the runner template** in `par-runners` from `par-runner-<ver>.qcow2` into the first free VMID of the
   reserved IDs at the end of the range (`qm create` + `qm set --scsi0 <storage>:0,import-from=<file>`), with a
   cloud-init drive and the guest agent enabled. Tag it `par-managed`, `par-template`,
   `par-tv-<import time in Unix seconds>`, `par-rv-<actions/runner version>` (from the image's manifest), and
   `par-release-<ver>`, and convert it to a template. See *Runner image*.
10. **Create the gateway VM** in `par-system` from `par-gateway-<ver>.qcow2`: 1 vCPU, 1 GiB RAM, a 6 GiB disk (the
    image's size), `net0` on the LAN bridge, and `net1` on `parnet`. Use the built-in cloud-init drive for hostname
    and the LAN address only, tag it `par-managed,par-gateway,par-release-<ver>`, and start it. Once it has finished
    booting (the guest agent answers and `systemctl is-system-running --wait` returns), pipe the worker subnet and
    the ranges to block (the LAN bridge's networks, every address the host has on any interface, and the controller
    VM) into `par-gateway-configure` through `qm guest exec --pass-stdin` (see *Gateway and controller images*),
    then check that it reaches the internet.
11. **Create the controller VM** in `par-system` from `parcon-<ver>.qcow2`: 2 vCPU, 2 GiB RAM, a 6 GiB disk (the
    image's size). Use Proxmox's built-in cloud-init drive for hostname and network only (no user data, no
    snippets), tag it `par-managed,par-controller`, and start it. Once it has an address, configure the gateway again
    to block it.
12. **Configure the controller** through the guest agent once it has finished booting. Secrets go through
    `qm guest exec --pass-stdin`, so they never appear on a command line or on the host's disk:
    - `/etc/proxmox-actions-runners/config.yaml`, rendered from the settings, with no `github.app` yet (see
      *Controller VM contract*)
    - the Proxmox token in `/etc/proxmox-actions-runners/pve-token`
    - with Proxmox's own certificate, a copy of the node's CA (`/etc/pve/pve-root-ca.pem`) in
      `/etc/proxmox-actions-runners/pve-ca.pem`
    The controller connects to the API by IP and verifies its certificate against a DNS name the certificate lists
    (`tlsServerName`), so renewals don't break it. It verifies Proxmox's own certificate against the node's CA
    (`caCertFile`), and a custom or ACME certificate the host's system CAs trust against the controller's system CAs.
    Only a certificate from a CA the host doesn't trust is pinned by fingerprint (`tlsFingerprint`), which preflight
    warns about: after it is renewed, `parcon config apply` pins the new one. `parcon` then runs
    `parcon check proxmox` in the VM as `parcon`. It confirms the Proxmox VE version, that the token has every
    privilege it needs on the pool, storage, and VNet, and that the storage accepts VM disks.
13. **Create the GitHub App** with the manifest flow described under *GitHub App setup*. The code the user pastes is
    passed to `parcon github app create` in the controller VM, so the App's private key goes straight from GitHub
    into the controller VM and never passes through the host. `parcon` then waits for the user to install the App
    (`parcon github app wait-installation`), learns from the installation which organization or repository the
    runners serve, and saves it and the installation ID in the settings and the controller's config. It saves the
    Client ID as soon as the key is in the VM, so a re-run after a failure keeps waiting for the same App rather than
    creating another. It then runs `parcon check github` in the VM, which confirms that the App credentials produce
    an installation token and can reach the org or repo, and enables and starts `parcon.service`.
14. **Worker network check** (also `parcon check network`). `parcon` clones one worker from the template into a
    reserved VMID, tagged `par-managed,par-build` so the reconcile loop leaves it alone, waits for it to finish
    booting, and confirms through `qm guest exec` that the worker got a DHCP lease, reaches GitHub, and can't reach
    the Proxmox API or the controller VM. It then destroys the clone, and waits for the controller to log
    `scale set session opened`, which it does once the scale set is registered and its listener session is open. A
    re-run first destroys a clone a failed run left.
15. **Finish.** Tag the controller VM `par-release-<ver>`, which marks a finished install, and print the `runs-on:`
    label, the controller and gateway VMs' IDs and IPs, and the `parcon` commands.

If any step fails, `parcon` stops and says why; fix the cause and run `parcon install` again to continue from there.
Each step checks for existing objects before it creates anything.

## Gateway and controller images

Both images start from the same pinned Ubuntu 26.04 cloud image as the runner image, turn on `unattended-upgrades`,
and include `qemu-guest-agent`, which is how `parcon` configures them. Both disks are 6 GiB, the VMs'
size. A built image holds about 2.5 GiB and grows only by logs and OS updates, but a kernel update from
`unattended-upgrades` briefly needs room for two kernels and a new initramfs, which 4 GiB can't hold.

### Gateway

The gateway runs dnsmasq for DHCP and DNS on `net1` and nftables for NAT and the firewall. The worker NIC is always
called `net1` inside the VM: the image names it by the PCI slot Proxmox gives `net1`, so nothing depends on MACs.

`parcon` configures it by piping `KEY=value` lines into `/usr/local/sbin/par-gateway-configure` through
`qm guest exec --pass-stdin`:

```
WORKER_SUBNET=10.251.0.0/22
BLOCK=192.0.2.0/24 192.0.2.10 192.0.2.11
```

| Key | Default | Meaning |
| --- | ------- | ------- |
| `WORKER_SUBNET` | `10.251.0.0/22` | The worker network's IPv4 subnet, a `/8` to a `/29` |
| `BLOCK` | empty | Space-separated IPv4 addresses and CIDRs workers must not reach: the LAN, every address of the Proxmox host, and the controller VM |

The command saves the settings to `/etc/par-gateway/config`, renders the config below from them, and reloads
dnsmasq and nftables. Each run replaces the previous settings, so a key that is left out gets its default, and a
second run with the same input changes nothing. Invalid input is refused before anything changes. The image ships
configured with the defaults.

- `net1` gets the subnet's first address. dnsmasq hands out the rest of the subnet with 1-hour leases and forwards
  DNS to the gateway's own upstream resolvers. It binds only to `net1`, so it doesn't clash with systemd-resolved.
- nftables (`/etc/nftables.conf`, loaded at boot):
  - forwards only new IPv4 connections from `net1` out of the LAN NIC, masqueraded
  - drops forwarded traffic to RFC 1918, CGNAT (`100.64.0.0/10`), link-local, and the `BLOCK` ranges
  - accepts only DHCP and DNS from `net1` to the gateway itself
  - forwards no IPv6, and IPv6 forwarding is off
- Strict reverse-path filtering (`rp_filter = 1`) drops any packet whose source doesn't route back out the NIC it
  arrived on, so workers can't spoof outside addresses. If the worker NIC ever isn't named `net1`, no `.network`
  file matches it and it stays down.

### Controller

The controller image follows the *Controller VM contract*. On top of it:

- `parcon` is statically linked and baked into the image at `/usr/local/bin/parcon`.
- The image creates the `parcon` user and `/etc/proxmox-actions-runners`, and installs `parcon.service`
  ([`deploy/parcon.service`](../deploy/parcon.service)) without enabling it; `parcon install` enables it in step 13.
  The unit starts only once `/etc/proxmox-actions-runners/config.yaml` exists.
- The image doesn't enforce the files' modes: `parcon` writes each one as `parcon` with `umask 077`, for example
  `runuser -u parcon -- sh -c 'umask 077; cat > FILE'`.

## GitHub App setup

GitHub Apps can only be created in a browser: there is no API for it, and the OAuth device flow (where you type a
code shown by a CLI) only works for an App that already exists. `parcon install` therefore uses GitHub's
[manifest flow](https://docs.github.com/en/apps/sharing-github-apps/registering-a-github-app-from-a-manifest) with
a static helper page, and the user copies one line back:

1. `parcon` picks a random `state`, 32 bytes from the kernel's random source in unpadded base64url (43 characters, 256
   bits, so nobody can guess it), and prints a link to the helper page with it:
   `https://klponce.github.io/proxmox-actions-runners/app/v1/?state=<state>`. The user opens it on any device with a
   browser. The page refuses a link without a state of that shape.
2. The page asks one question, where the runners should work:
   - **An organization**, by name: its runners serve all of its repositories. The page posts to
     `https://github.com/organizations/<org>/settings/apps/new?state=<state>`, and the App asks for
     **Self-hosted runners: write** on the organization.
   - **My personal account**: GitHub allows self-hosted runners on a personal account only per repository, so the
     App asks for **Administration: write** on repositories, and the user picks the repository when installing it.
     The page posts to `https://github.com/settings/apps/new?state=<state>`, which creates the App in the signed-in
     account.

   The manifest also sets a name, `par-runners-<8 random hex digits>` (App names are unique across GitHub), `url`
   and an inactive webhook (`hook_attributes: {url: <project repository>, active: false}`), because the controller
   long-polls, no events, `redirect_url` back to the same page, and `public: false`. The page shows what it will
   send under *What is sent to GitHub*.
3. The user reviews the App on GitHub and clicks **Create GitHub App**. GitHub redirects back to the page with `code`
   and `state`. The page shows one line to copy, `<state>.<code>`, removes the code from the address bar, and warns
   that the line unlocks the App's private key for up to an hour.
4. The user pastes the line into `parcon`, which splits it at the first `.`, checks that the state is
   the one it printed, and pipes **only the code** into `parcon github app create` in the controller VM.
5. `parcon` prints the App's install link (`https://github.com/apps/<slug>/installations/new`), GitHub's own
   install page, where the user installs the App on the organization, or on the repository for a personal account.
   A private App can only be installed on the account that owns it, so the App and its installation can't disagree.
   `parcon github app wait-installation` polls until the installation appears. No second copy-back is needed.
6. `parcon` takes the runners' target from the installation: the organization, or the one repository the App can
   reach on a personal account. If it can reach several, `--github-url` picks one, or `parcon` asks. With
   `--github-url` set, the installation must match it. `parcon` saves the target and the installation ID in the
   settings, renders `configUrl`, `clientId`, `installationId`, and `privateKeyFile` into `github` in `config.yaml`,
   then runs `parcon check github`.

The `parcon` commands, run in the controller VM as `parcon`. Secrets come only on stdin, never as arguments, and are
never logged or printed:

| Command | Stdin | Stdout |
| ------- | ----- | ------ |
| `parcon github app create [-key-file PATH]` | the manifest code | `{"clientId":"…","appId":…,"slug":"…","owner":"…","ownerType":"Organization"}` |
| `parcon github app import [-key-file PATH]` | an existing App's PEM private key | nothing |
| `parcon github app wait-installation -client-id ID [-key-file PATH] [-timeout 15m]` | nothing | `{"installationId":…,"account":"…","accountType":"User","repositories":["…"]}` |

- `-key-file` defaults to `/etc/proxmox-actions-runners/github-app.pem`. `create` and `import` write it with mode
  `0600` through a temporary file in the same directory and a rename, so a reader never sees half a key. `create`
  creates the temporary file before it exchanges the code, so a key path it can't write fails without spending the
  single-use code.
- `create` calls `POST /app-manifests/{code}/conversions` and keeps only the App ID, slug, Client ID, owner, and
  private key. It drops the client secret and webhook secret, which the controller doesn't use. An invalid, used, or
  expired code fails with a message that says so.
- `import` is the `--app manual --client-id ID` path for an App that already exists: `parcon install` reads its
  private key from the terminal and pipes the key to `import`. `import` checks that it is a PEM-encoded RSA private key.
- `wait-installation` authenticates as the App with a JWT and polls `GET /app/installations` every 5 seconds until
  the App is installed. On a personal account it also lists the repositories the installation can reach
  (`GET /installation/repositories` with an installation token), since the runners need one of them; an
  organization's runners serve the whole organization. Errors don't end the wait, because a network blip, a GitHub
  error, or a newly created App that GitHub doesn't know yet all pass. It prints each new error as it happens and
  fails after `-timeout` with the last one.

About the helper page:

- It lives in `site/app/v1/index.html` and is published with GitHub Pages by `.github/workflows/pages.yml`. It is
  plain HTML and JavaScript with no third-party scripts, analytics, or cookies. Its Content-Security-Policy allows
  no requests except the form `POST` to GitHub.
- Its path is versioned (`/app/v1/`). Once a release has shipped, a change to the manifest or the query parameters
  gets a new path, so older releases keep working.
- The code on the page can be exchanged for the App's private key by anyone who has it, until it is used or expires.
  The page says so, and `parcon` exchanges it at once and never logs it.

Typing a code from `parcon` into the page (the device-flow style) isn't possible: the page is static, so it has no
way to send the result back to `parcon`. That would need a hosted relay service, which would also see
the code that unlocks the private key.

## Metrics endpoint

*Deferred past v0.1.* Neither image includes nginx yet, and `parcon` doesn't serve metrics yet. The design:

`parcon` serves `/metrics`, `/healthz`, and `/readyz` as plain HTTP on `127.0.0.1:9465` only. nginx (from Ubuntu
main, so `unattended-upgrades` patches it) runs in the controller VM and serves them over HTTPS on the LAN at
`https://<controller-ip>:9464/`.

- The certificate is self-signed (ECDSA P-256) with the VM's hostname and LAN addresses as subject alternative
  names. It is generated inside the controller VM on first boot, never baked into the image, since every install
  would then share the same key. A timer regenerates it 30 days before it expires, which is 2 years after it was
  issued.
- `parcon status` prints the certificate's SHA-256 fingerprint so Prometheus can be pointed at it with `ca_file`, or
  at least checked by hand.
- There is no authentication for now. The endpoint exposes counts, latencies, and scale set names, but no secrets.
  Workers can't reach it because the gateway blocks the LAN.
- `/readyz` fails while the GitHub message session or the Proxmox API is unreachable. `/healthz` only checks that the
  process is running.

## Runner image

The runner template is imported, never built on the node. CI builds `par-runner-<ver>.qcow2` with Packer from
`images/runner/`: it starts from the pinned Ubuntu 26.04 cloud image, installs the runner and the one-job units,
runs the checks in `images/runner/tests/`, and cleans up the machine-id, SSH host keys, cloud-init state, and logs.
The image's disk size is the baseline that each worker's `freeDiskGiB` is added to, so it is kept small.

Templates are immutable. `parcon update` imports each new release's runner image as a new template with a newer `par-tv` tag,
and the controller clones the newest one. Linked clones depend on their template, so the controller deletes an old
template only once no worker references it (`par-tpl-<VMID>` on each worker). A `par-build` smoke-test clone doesn't
say which template it came from, so the controller deletes no template while one exists. `parcon` destroys its
clones itself, and `parcon uninstall` removes any that remain.

GitHub stops accepting a runner with auto-update disabled 30 days after a newer `actions/runner` release. Each
runner release therefore ships as a project release with a new runner image, and `parcon update` imports it. A
daily workflow (`.github/workflows/runner-release.yml`) opens a pull request that bumps the image's pin as soon as a
new `actions/runner` release is out.
The controller logs a warning when the template has been behind the latest release for 7 days and an error after
21, and `parcon status` and `parcon check template` report the same.

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
  `parcon` can't check.

The gateway is a single point of failure for worker networking. `parcon status` shows whether its DHCP, DNS, and
NAT services run, and `parcon check network` tests the network end to end from a throwaway worker. Its OS updates
itself with `unattended-upgrades`, and `parcon update` replaces it with the new release's image.

### Host NIC offloads

The node may itself be a VM, for example to keep the runners apart from a main Proxmox setup. Its NIC is then a
virtio NIC under the LAN bridge, and its offloads (GRO, GSO, TSO, and TX checksumming) are on by default. The NIC
merges incoming packets that the bridge then forwards to the gateway VM, which NATs them to the workers, and
workers' downloads stall at a few KB/s while the host's own run at full speed.

On such a node (a port of the LAN bridge whose driver is `virtio_net`), `parcon install` and `parcon update` turn
those offloads off with `ethtool -K`, and write `/etc/udev/rules.d/90-par-offloads.rules` so they stay off after a reboot. The rule
matches the NIC by the name Proxmox gives it (such as `nic0`) and sorts after `80-net-setup-link.rules`, which
renames it. This is a file of our own rather than a `post-up` line in `/etc/network/interfaces`, which Proxmox's GUI
rewrites. `parcon check` and `parcon status` warn while the offloads are on or the rule is missing, and
`parcon uninstall` removes the rule and turns the offloads back on. On a node that isn't a VM, none of this happens.

## Update and uninstall

Each VM `parcon` creates carries a `par-release-<version>` tag, and the controller VM gets its tag last, so it marks
a finished install or update. `parcon install` on a node with a finished install of an older release updates it; on
one with an unfinished install (the `par-system` pool exists), it continues it.

- **Update** (`parcon update`) is in place, run on the host:
  - It finds the newest release (with `--pre`, the newest pre-release too). If that is newer than the host's
    `parcon`, it shows the plan, verifies the release's signature, replaces `/usr/local/bin/parcon` with the
    release's, and runs it to do the rest, so each release's own code updates the node to it.
  - The release's runner image is imported as a new template, and the controller removes the old one once its
    workers are gone.
  - The gateway VM holds no state beyond what the settings say, so `parcon` replaces it with the new image under the
    same VMID, NICs, and address, and configures it with `par-gateway-configure`. Workers lose their network for the
    minute or two this takes.
  - The controller VM downloads the release's `parcon-<ver>-linux-amd64` itself and checks it against the checksum
    from the verified `SHA256SUMS`, which `parcon` passes it through the guest agent. The binary is far too big for
    the guest agent to carry, and config and secrets never leave the VM. `parcon` then pushes the config the new
    release renders, restarts `parcon.service`, and waits for its session. The VM's OS updates itself with
    `unattended-upgrades`.
  - On a node that is itself a VM, the LAN NIC's offloads are turned off if they aren't yet (see *Host NIC
    offloads*).
  - Each piece already at the release is left alone, so `parcon update` again continues an update that stopped. An
    install newer than the host's `parcon` is refused rather than downgraded.
- **Uninstall** (`parcon uninstall`) first stops the controller and deletes the scale set in GitHub
  (`parcon github scaleset delete` in the controller VM, which also unregisters its runners). It then destroys every
  VM tagged `par-managed` in the two pools, clones before templates, removes the ACLs, token, user, role, and pools,
  and removes the `parzone` zone and `parnet` VNet and applies the SDN config. On a node that is itself a VM, it
  removes the udev rule that keeps the LAN NIC's offloads off and turns them back on. Last, it removes the settings
  and `/usr/local/bin/parcon`. It doesn't touch anything it didn't create. The GitHub App stays; delete it in
  GitHub's settings if you no longer need it.

## Conventions

- The host commands live in `internal/installer`, and reach the host only through `pvecli` (commands) and `hostsys`
  (files and facts), so their tests run whole installs, updates, and uninstalls against a fake node.
- Every change to the host goes through `pvecli.Changer`, which logs it and honors `--dry-run`. Secrets are never
  arguments, so the log never holds one: they go to the controller VM on `qm guest exec`'s stdin. A test runs a whole
  install with sentinel secrets and checks every command line, output, error, and host file for them.
- Use only tools that ship with Proxmox VE 9, and parse their JSON output in Go.
- `install/install.sh` stays a bootstrap in Bash with `set -euo pipefail`. Its body is inside `main` and called on the
  last line, so a partial `curl | bash` download can't run half a script. It must pass `shellcheck`, and its bats
  tests (`install/tests`) stub `curl` and the checks it makes.
