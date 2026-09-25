# Installer design

The whole project installs with one script, [`install/install.sh`](../install/install.sh), run as root on a
Proxmox VE 9 node. The script checks the host, creates an isolated worker network with a gateway VM, creates a small
controller VM, imports the runner template, and registers the scale set. It changes the host only through Proxmox's
own tools, and it can remove everything it created. It installs only from a release: the release workflow writes
the version and the assets' checksums into it, and a copy from the source tree refuses to install.

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

Without flags, the script prompts for the settings that have no default. For unattended installs, pass an answers
file of `KEY=value` lines; `bash install.sh --help` lists the keys and their defaults. Values are read literally,
never evaluated.

```bash
cat >par.env <<'EOF'
PAR_GITHUB_URL=https://github.com/my-org
PAR_MAX_RUNNERS=4
PAR_STORAGE=local-zfs
EOF
bash install.sh --answers par.env --yes
```

| Command or flag | Effect |
| --------------- | ------ |
| `install` (default) | Install, or upgrade if an install is found |
| `upgrade` | Upgrade to the script's release: the controller's `parcon`, the gateway VM, and a new runner template. Keeps config and secrets |
| `uninstall` | Remove all managed VMs, templates, the controller VM, and the Proxmox objects |
| `check` | Run the preflight checks only |
| `--dry-run` | Run the checks and print the plan, then exit without changing anything |
| `--answers <file>` | Read settings from a `KEY=value` file instead of prompting |
| `--yes` | Skip the confirmation prompts |

Each `install.sh` installs exactly its own release. To install another version, download that release's
`install.sh`.

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

Each release publishes the following files, built by `.github/workflows/release.yml` when a `vX.Y.Z` tag is
pushed. Each image is boot-tested before it is published. The installer contains the SHA-256 of every asset for its
own version and refuses a file that doesn't match.

| Asset | Contents | Built by |
| ----- | -------- | -------- |
| `install.sh` | The installer | Release workflow |
| `parcon-<ver>.qcow2` | The controller VM: Ubuntu 26.04, `qemu-guest-agent`, `unattended-upgrades`, the `parcon` binary, user, and unit | Packer `qemu` builder in CI (`images/controller/`) |
| `par-gateway-<ver>.qcow2` | The gateway VM: Ubuntu 26.04, `qemu-guest-agent`, `unattended-upgrades`, nftables, dnsmasq, `par-gateway-configure` | Packer `qemu` builder in CI (`images/gateway/`) |
| `par-runner-<ver>.qcow2` | The runner template: Ubuntu 26.04, `qemu-guest-agent`, the `runner` user, a pinned `actions/runner`, Docker, and a few basics | Packer `qemu` builder in CI (`images/runner/`) |
| `par-runner-<ver>.json` | The runner image's build manifest, including its `actions/runner` version | Packer `qemu` builder in CI |
| `parcon-<ver>-linux-amd64` | The `parcon` binary, which `upgrade` puts in the controller VM | Release workflow |
| `SHA256SUMS` | Checksums of all of the above, `install.sh` included | Release workflow |

The release workflow writes the version and the other assets' checksums into `install.sh` (its `@PAR_VERSION@` and
`@PAR_SHA256SUMS@` placeholders, with `install/fill-release.sh`), so the script is the trust anchor for every asset it
downloads. A tag with a suffix, such as `v0.2.0-rc.1`, publishes a pre-release: its `install.sh` installs it, but the
`releases/latest` links in *Usage* keep pointing at the latest full release.

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
| Free space | 50 GiB free on the VM storage: the gateway, controller, and template disks, and room for one worker | hard |
| Download space | 6 GiB free in `/var/tmp`, on the host's root filesystem, where the images are downloaded before they are imported | hard |
| Free memory and CPU | host RAM and threads against `maxRunners` × worker size plus existing VMs | warn |
| LAN bridge | the bridge for the controller and gateway VMs exists. VLAN tag valid if set | hard |
| API certificate | how the controller will verify it: the node's CA, the system CAs, or a pinned fingerprint for a certificate from a CA the host doesn't trust | warn if pinned |
| SDN available | `ifupdown2` installed and `/etc/network/interfaces` sources `/etc/network/interfaces.d/*`, so applying SDN works | hard |
| Worker subnet free | the worker subnet doesn't overlap any route or address on the host, or the LAN subnet given for the gateway | hard |
| SDN names free | zone `parzone` and VNet `parnet` are unused, or already ours (upgrade) | hard |
| No name or ID clash | pools, user, role, and VMIDs are unused, or already tagged as ours (upgrade) | hard |
| Pending SDN changes | no SDN zone or VNet other than ours has pending changes, since applying ours would apply theirs too. Uninstall doesn't apply while any exist, and says so | hard |

## Controller VM contract

The controller image and the installer agree on this layout:

- **User:** a system user `parcon` with no login shell, created by the controller image. `parcon run` runs as
  `parcon` from a systemd unit.
- **Binary:** `/usr/local/bin/parcon`, baked into the controller image. An upgrade replaces the file and restarts the
  unit.
- **Config and secrets:** the directory `/etc/proxmox-actions-runners` is owned by `parcon:parcon` with mode `0700`.
  Every file in it is owned by `parcon` with mode `0600`: `config.yaml`, `pve-token`, and `github-app.pem`.
- **Commands:** the installer runs every `parcon` command in the VM as `parcon`, with
  `runuser -u parcon -- parcon …` through `qm guest exec`, so files that `parcon` writes get the right owner.
- **Before the App exists:** `github.app` (`clientId`, `installationId`, `privateKeyFile`) may be left out of
  `config.yaml`, because step 10 checks Proxmox before step 11 creates the App. `parcon check config`,
  `parcon check proxmox`, and `parcon check template` work without it. `parcon run` and `parcon check github` fail
  with "the GitHub App isn't set up yet" until all three fields are set.
- **Who writes the config:** only the installer. `parcon` prints the non-secret values it learns (client ID, App ID,
  slug, installation ID) and writes secrets only to their own files. Nothing in `parcon` writes YAML.

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
7. **Import the runner template** in `par-runners` from `par-runner-<ver>.qcow2` into the first free VMID of the
   reserved IDs at the end of the range (`qm create` + `qm set --scsi0 <storage>:0,import-from=<file>`), with a
   cloud-init drive and the guest agent enabled. Tag it `par-managed`, `par-template`,
   `par-tv-<import time in Unix seconds>`, and `par-rv-<actions/runner version>` (from the image's manifest), and
   convert it to a template. See *Runner image*.
8. **Create the gateway VM** in `par-system` from `par-gateway-<ver>.qcow2`: 1 vCPU, 1 GiB RAM, a 6 GiB disk (the
   image's size), `net0` on the LAN bridge, and `net1` on `parnet`. Use the built-in cloud-init drive for hostname
   and the LAN address only, tag it `par-managed,par-gateway`, and start it. Once the guest agent responds, pipe
   the worker subnet and the ranges to block (the LAN bridge's networks, every address the host has on any
   interface, and the controller VM) into `par-gateway-configure` through `qm guest exec --pass-stdin` (see
   *Gateway and controller images*), then check that it serves DHCP on `parnet` and reaches the internet.
9. **Create the controller VM** in `par-system` from `parcon-<ver>.qcow2`: 2 vCPU, 2 GiB RAM, a 6 GiB disk (the
   image's size). Use Proxmox's built-in cloud-init drive for hostname and network only (no user data, no
   snippets), tag it `par-managed,par-controller`, and start it.
10. **Configure the controller** through the guest agent once it responds. Secrets go through
    `qm guest exec --pass-stdin`, so they never appear on a command line or on the host's disk:
    - `/etc/proxmox-actions-runners/config.yaml` with the settings, but no `github.app` yet (see *Controller VM
      contract*)
    - the Proxmox token in `/etc/proxmox-actions-runners/pve-token`
    - with Proxmox's own certificate, a copy of the node's CA (`/etc/pve/pve-root-ca.pem`) in
      `/etc/proxmox-actions-runners/pve-ca.pem`
    The files are owned by `parcon` with mode `0600`. The controller connects to the API by IP and verifies its
    certificate against a DNS name the certificate lists (`tlsServerName`), so renewals don't break it. It verifies
    Proxmox's own certificate against the node's CA (`caCertFile`), and a custom or ACME certificate the host's
    system CAs trust against the controller's system CAs. Only a certificate from a CA the host doesn't trust is
    pinned by fingerprint (`tlsFingerprint`), which preflight warns about: its renewal needs a config edit. The installer then runs `parcon check proxmox` in the VM as
    `parcon`. It confirms the Proxmox VE version, that the token has every privilege it needs on the pool, storage,
    and VNet, and that the storage accepts VM disks.
11. **Create the GitHub App** with the manifest flow described under *GitHub App setup*. The code the user pastes is
    passed to `parcon github app create` in the controller VM, so the App's private key goes straight from GitHub
    into the controller VM and never passes through the host. The installer then waits for the user to install the
    App (`parcon github app wait-installation`) and writes the App's installation ID into `config.yaml`. It writes
    the Client ID there as soon as the key is in the VM, so a re-run after a failure keeps waiting for the same App
    rather than creating another. The installer then runs `parcon check github` in the VM. This confirms that the App
    credentials produce an installation token and can reach the org or repo. It then enables and starts
    `parcon.service`.
12. **Smoke test.** The installer clones one worker from the template into a reserved VMID, tagged
    `par-managed,par-build` so the reconcile loop leaves it alone, confirms the guest agent responds, and confirms
    through `qm guest exec` that the worker got a DHCP lease, reaches GitHub, and can't reach the Proxmox API or the
    controller VM. It then destroys the clone and waits for the controller to log `scale set session opened`, which
    it does once the scale set is registered and its listener session is open. A re-run first destroys a clone a
    failed run left.
13. **Summary.** Print the `runs-on:` label, the controller and gateway VMs' IDs and IPs, and the upgrade and
    uninstall commands. (The metrics URL and its certificate's fingerprint come with the metrics endpoint, which is
    deferred.)

If any step fails, the installer stops and prints what it already created, so a re-run can continue from there.
Each step checks for existing objects before it creates anything.

## Gateway and controller images

Both images start from the same pinned Ubuntu 26.04 cloud image as the runner image, turn on `unattended-upgrades`,
and include `qemu-guest-agent`, which is how the installer configures them. Both disks are 6 GiB, the VMs'
size. A built image holds about 2.5 GiB and grows only by logs and OS updates, but a kernel update from
`unattended-upgrades` briefly needs room for two kernels and a new initramfs, which 4 GiB can't hold.

### Gateway

The gateway runs dnsmasq for DHCP and DNS on `net1` and nftables for NAT and the firewall. The worker NIC is always
called `net1` inside the VM: the image names it by the PCI slot Proxmox gives `net1`, so nothing depends on MACs.

The installer configures it by piping `KEY=value` lines into `/usr/local/sbin/par-gateway-configure` through
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
  ([`deploy/parcon.service`](../deploy/parcon.service)) without enabling it; the installer enables it after step 11.
  The unit starts only once `/etc/proxmox-actions-runners/config.yaml` exists.
- The image doesn't enforce the files' modes: the installer writes each one as `parcon` with `umask 077`, for example
  `runuser -u parcon -- sh -c 'umask 077; cat > FILE'`.

## GitHub App setup

GitHub Apps can only be created in a browser: there is no API for it, and the OAuth device flow (where you type a
code shown by a CLI) only works for an App that already exists. The installer therefore uses GitHub's
[manifest flow](https://docs.github.com/en/apps/sharing-github-apps/registering-a-github-app-from-a-manifest) with
a static helper page, and the user copies one line back:

1. The installer picks a random `state` (letters, digits, `-` and `_`, no `.`) and prints a link to the helper page,
   `https://klponce.github.io/proxmox-actions-runners/app/v1/`, with one of these query strings:

   | Runners for | Query string |
   | ----------- | ------------ |
   | an organization | `?org=<org>&state=<state>` |
   | a repository owned by an organization | `?org=<org>&repo=<repo>&state=<state>` |
   | a repository owned by a personal account | `?user=<user>&repo=<repo>&state=<state>` |

   The user opens it on any device with a browser.
2. The page builds the manifest and submits it as a form `POST` to
   `https://github.com/organizations/<org>/settings/apps/new?state=<state>`, or
   `https://github.com/settings/apps/new?state=<state>` for a repository owned by a personal account (the user signs
   in as that account). The manifest sets:
   - a name, `par-<owner>-<6 random hex digits>`, since App names are unique across GitHub (the owner is shortened to
     fit GitHub's 34-character limit)
   - `url` and an inactive webhook (`hook_attributes: {url: <project repository>, active: false}`), because the
     controller long-polls
   - the one permission the target needs: **Self-hosted runners: write** for an organization, or
     **Administration: write** for a repository. Write includes read.
   - no events, `redirect_url` back to the same page, and `public: false`
   The page shows the manifest it will send under *What is sent to GitHub*.
3. The user reviews the App on GitHub and clicks **Create GitHub App**. GitHub redirects back to the page with `code`
   and `state`. The page shows one line to copy, `<state>.<code>`, removes the code from the address bar, and warns
   that the line unlocks the App's private key for up to an hour.
4. The user pastes the line into the installer. The installer splits it at the first `.`, checks that the state is
   the one it printed, and pipes **only the code** into `parcon github app create` in the controller VM.
5. The installer prints the App's install link (`https://github.com/apps/<slug>/installations/new`), and the user
   installs the App on the organization or repository. `parcon github app wait-installation` polls until the
   installation appears. No second copy-back is needed.
6. The installer writes `clientId`, `installationId`, and `privateKeyFile` into `github.app` in `config.yaml`, then
   runs `parcon check github`.

The `parcon` commands, run in the controller VM as `parcon`. Secrets come only on stdin, never as arguments, and are
never logged or printed:

| Command | Stdin | Stdout |
| ------- | ----- | ------ |
| `parcon github app create [-key-file PATH]` | the manifest code | `{"clientId":"…","appId":…,"slug":"…"}` |
| `parcon github app import [-key-file PATH]` | an existing App's PEM private key | nothing |
| `parcon github app wait-installation -client-id ID -target URL [-key-file PATH] [-timeout 15m]` | nothing | the installation ID |

- `-key-file` defaults to `/etc/proxmox-actions-runners/github-app.pem`. `create` and `import` write it with mode
  `0600` through a temporary file in the same directory and a rename, so a reader never sees half a key. `create`
  creates the temporary file before it exchanges the code, so a key path it can't write fails without spending the
  single-use code.
- `create` calls `POST /app-manifests/{code}/conversions` and keeps only the App ID, slug, Client ID, and private
  key. It drops the client secret and webhook secret, which the controller doesn't use. An invalid, used, or expired
  code fails with a message that says so.
- `import` is the `PAR_GITHUB_APP=manual` path for an App that already exists: the installer asks for its Client ID,
  reads its private key, and pipes the key to `import`. `import` checks that it is a PEM-encoded RSA private key.
- `wait-installation` authenticates as the App with a JWT and polls `GET /orgs/{org}/installation` or
  `GET /repos/{owner}/{repo}/installation` every 5 seconds until the App is installed on `-target`
  (`https://github.com/<org>` or `https://github.com/<owner>/<repo>`, the same as `github.configUrl`). Errors don't
  end the wait, because a network blip, a GitHub error, or a newly created App that GitHub doesn't know yet all
  pass. It prints each new error as it happens and fails after `-timeout` with the last one.

About the helper page:

- It lives in `site/app/v1/index.html` and is published with GitHub Pages by `.github/workflows/pages.yml`. It is
  plain HTML and JavaScript with no third-party scripts, analytics, or cookies. Its Content-Security-Policy allows
  no requests except the form `POST` to GitHub.
- Its path is versioned (`/app/v1/`). A change to the manifest or the query parameters gets a new path, so older
  installers keep working.
- The code on the page can be exchanged for the App's private key by anyone who has it, until it is used or expires.
  The page says so, and the installer exchanges it at once. Neither the installer nor `parcon` logs it.

Typing a code from the installer into the page (the device-flow style) isn't possible: the page is static, so it
has no way to send the result back to the installer. That would need a hosted relay service, which would also see
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
- The installer prints the certificate's SHA-256 fingerprint so Prometheus can be pointed at it with `ca_file`, or
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

Templates are immutable. The installer imports each new runner image as a new template with a newer `par-tv` tag,
and the controller clones the newest one. Linked clones depend on their template, so the controller deletes an old
template only once no worker references it (`par-tpl-<VMID>` on each worker). A `par-build` smoke-test clone doesn't
say which template it came from, so the controller deletes no template while one exists. The installer destroys
its clones itself, and `uninstall` removes any that remain.

GitHub stops accepting a runner with auto-update disabled 30 days after a newer `actions/runner` release. Each
runner release therefore ships as a project release with a new runner image, and `install.sh upgrade` imports it. A
daily workflow (`.github/workflows/runner-release.yml`) opens a pull request that bumps the image's pin as soon as a
new `actions/runner` release is out.
The controller logs a warning when the template has been behind the latest release for 7 days and an error after
21, and `parcon check template` reports the same.

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

Each VM the installer creates carries a `par-release-<version>` tag, and the controller VM gets its tag last, so
it marks a finished install. `install` on a node with a finished install upgrades it; on one with an unfinished
install (the `par-system` pool exists), it continues it.

- **Upgrade** is in place, run on the host like the install:
  - The controller VM downloads the release's `parcon-<ver>-linux-amd64` itself, checks it against the checksum the
    installer passes it through the guest agent, replaces `/usr/local/bin/parcon`, and restarts `parcon.service`.
    The binary is far too big for the guest agent to carry, and config and secrets never leave the VM. The VM's OS
    updates itself with `unattended-upgrades`.
  - The gateway VM holds no state beyond its settings, so the installer reads them, replaces the VM with the new
    image under the same VMID and address, and reconfigures it with `par-gateway-configure`. Workers lose their
    network for the minute or two this takes.
  - The release's runner image is imported as a new template, and the controller removes the old one once its
    workers are gone.
- **Uninstall** first stops the controller and deletes the scale set in GitHub (`parcon github scaleset delete` in
  the controller VM, which also unregisters its runners). It then destroys every VM tagged `par-managed` in the two
  pools, clones before templates, removes the ACLs, token, user, role, and pools, and removes the `parzone` zone and
  `parnet` VNet and applies the SDN config. It doesn't touch anything it didn't create. The GitHub App stays; delete
  it in GitHub's settings if you no longer need it.

## Script conventions

- Bash with `set -euo pipefail`. The whole body is inside `main` and called on the last line, so a partial
  `curl | bash` download can't run half a script. Tests source the script with `PAR_INSTALL_SOURCED=1`, which skips
  `main`.
- Use only tools that ship with Proxmox VE 9. Parse JSON with `pvesh --output-format json` and `perl -MJSON`, not
  `jq`.
- Every command that changes the host goes through the `change` function, which logs it and honors `--dry-run`.
  Secrets are never arguments, so the log never holds one: they go to the controller VM on `qm guest exec`'s stdin.
- Must pass `shellcheck`. Tests use `bats` (`install/tests`) with stubbed `qm`, `pveum`, `pvesh`, `curl`, and `ip`.
  A Go test keeps the script's role privileges equal to what `parcon check proxmox` requires.
