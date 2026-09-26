# Integration suite

Runs the project against a real Proxmox VE 9 node. All it needs is root SSH access to a **throwaway** node: it
creates its own test objects, runs the Go integration tests and the `parcon` checks, and can remove everything
again. Given a runner image built from `images/runner`, it also runs a worker from that image, and given the
gateway and controller images too, it runs `install/install.sh` on the node up to the GitHub App.

GitHub itself is only partly covered: `parcon check template` looks up the latest `actions/runner` release on
github.com, but the controller tests use the in-memory GitHub fake, so no real runner registers.

## The node

- Proxmox VE 9.x, standalone (not in a cluster), with `/dev/kvm`. A nested node needs its CPU type set to `host`
  on the outer host.
- A bridge with DHCP and internet access (`vmbr0` by default), used only to build the test template.
- About 12 GiB of free space on the VM storage and 2 GiB of free memory, plus room for the runner image if you use
  one.
- Nothing you care about in the suite's VMIDs: 9000, 9001, and 9900–9910 by default. Setup refuses VMIDs used by
  anything but the suite.

The suite changes the node, so it only runs with `PAR_IT_THROWAWAY=1`.

## Running it

Run it inside the dev container, with your SSH key mounted into it:

```bash
devcontainer up --workspace-folder . \
  --mount "type=bind,source=$HOME/.ssh/<key>,target=/home/dev/.ssh-par-it/id_ed25519"
```

In a git worktree, add the `.git` mount from [AGENTS.md](../../AGENTS.md#development-environment). Then:

```bash
devcontainer exec --workspace-folder . \
  --remote-env PAR_IT_SSH=root@<node> \
  --remote-env PAR_IT_SSH_KEY=/home/dev/.ssh-par-it/id_ed25519 \
  --remote-env PAR_IT_THROWAWAY=1 \
  test/integration/run.sh
```

| Command | What it does |
| ------- | ------------ |
| `setup` | Preflight checks, then creates the suite's objects. Safe to re-run; it keeps an existing template |
| `test` | Runs the Go integration tests, the `parcon` checks, and the permission checks below |
| `teardown` | Removes everything `setup` created and restores the storage setting it changed |
| `all` | `setup`, then `test` (the default) |
| `installer` | Runs `parcon install` on the node without GitHub, then uninstalls it. See [The installer](#the-installer) |

`run.sh -h` lists the settings, such as `PAR_IT_STORAGE`, `PAR_IT_TEMPLATE_VMID`, and `PAR_IT_TEST_VMID`.

### With the runner image

Build the image in the dev container (it needs `/dev/kvm`, which the dev container passes through), then point the
suite at it. The `.json` manifest the build writes next to the image must stay there: the suite tags the template
from it, as the installer will.

```bash
packer init images/runner && packer build -var version=it images/runner
```

```bash
devcontainer exec --workspace-folder . ... \
  --remote-env PAR_IT_RUNNER_IMAGE=output-runner/par-runner-it.qcow2 \
  test/integration/run.sh
```

### The installer

`run.sh installer` tests `parcon`'s host commands on the node with locally built images and no GitHub App. It
needs all three images, built with the same version, and a node with no install on it:

```bash
packer init images/gateway && packer build -var version=it images/gateway
CGO_ENABLED=0 go build -o bin/parcon ./cmd/parcon
packer init images/controller && packer build -var version=it -var parcon_binary=bin/parcon images/controller
```

```bash
devcontainer exec --workspace-folder . ... \
  --remote-env PAR_IT_RUNNER_IMAGE=output-runner/par-runner-it.qcow2 \
  --remote-env PAR_IT_GATEWAY_IMAGE=output-gateway/par-gateway-it.qcow2 \
  --remote-env PAR_IT_CONTROLLER_IMAGE=output-controller/parcon-it.qcow2 \
  test/integration/run.sh installer
```

It builds `parcon` and copies it and the images, as release assets with a `SHA256SUMS`, to
`/var/tmp/par-it-installer` on the node, then:

1. runs `parcon install --dry-run` with the local assets (`--assets`, which trusts their `SHA256SUMS` instead of a
   signed release), and checks that it changed nothing on the node or its files;
2. runs `parcon install --stop-after configure`: parcon and its settings on the host, the Proxmox access objects, the
   worker network, the runner template, the gateway and controller VMs, and the controller's configuration, which
   ends with `parcon check proxmox` inside the controller VM;
3. runs `parcon check proxmox` on the host, which forwards it into the controller VM, and `parcon check network`: a
   worker cloned from the template gets a DHCP lease from the gateway, reaches the internet, and can't reach the
   Proxmox API or the controller VM;
4. stops the controller VM and runs `parcon uninstall --yes`, then checks that nothing of the install is left,
   `/usr/local/bin/parcon` and its settings included.

It stops before the GitHub App and doesn't start the controller service, whose scale set session needs the App. It
uses the install's real names (`par-runners`, `par-system`, `parzone`, `parnet`), so it refuses a node that already
has an install, and it uninstalls even when a step fails. The node needs the free space the install's checks ask
for: 50 GiB on the VM storage.

## What setup creates

| Object | Name |
| ------ | ---- |
| SDN simple zone and VNet | `parit`, `paritnet` |
| Pool | `par-it`, and `par-it-image` with a runner image |
| Role | `PARIntegration`: the privileges in `internal/proxmox/access.go`, plus `VM.GuestAgent.Unrestricted` so the tests can look inside workers |
| User and privilege-separated API token | `par-it@pve!it` |
| ACLs for the user and token | the pools, the storage, and the VNet |
| Test template | VMID 9000 in `par-it`: Ubuntu 26.04 with the QEMU guest agent, `/run/par-runner` created at boot, tagged like a runner template with the latest `actions/runner` release |
| Runner image template | with `PAR_IT_RUNNER_IMAGE`: VMID 9001 in `par-it-image`, imported and tagged from the image's manifest |

The names differ from a real install's (`parzone`, `parnet`, `par-runners`), so the suite can't touch one. The
runner image gets its own pool because the controller prunes every template in its pool except the newest, so
the two templates can't share one.

The test template is a test fixture, not how the product builds templates: it installs the guest agent with a
cloud-init snippet, which needs the `snippets` content type on the `local` storage. Teardown restores that setting.

## What test checks

1. `internal/proxmox` integration tests: version, permissions, VM listing (including that the token sees the
   template's pool), storage, and a clone → configure → grow disk → start → guest agent write and exec → destroy
   lifecycle.
2. `internal/controller` integration tests:
   - the controller creates a worker that records its template, writes the JIT config and then the empty ready
     file, adopts the worker after a restart, retires it when it powers off, and retires an idle worker on
     scale-down;
   - it prunes an older template that no worker uses and keeps the newest;
   - with the runner image, a worker cloned from it runs its runner once the JIT config arrives, powers itself off
     when the runner exits (it gets a config GitHub would reject), and is retired.
3. `parcon check proxmox` passes.
4. `parcon check template` finds the test template and reports its runner as the latest release, looked up on
   github.com. With the runner image, it also finds that template and reports the manifest's runner version.
5. `parcon check proxmox` fails without `Pool.Audit`, because the token then can't see which pool a VM is in.
6. `parcon check proxmox` fails when only the token, not its user, has the VNet ACL, because a privilege-separated
   token gets only what both have.

Steps 5 and 6 change the test role on purpose and put it back afterwards, even if a check fails.

## Private data

Everything the suite writes locally goes to `.agents/integration/` (mode 0700), which git ignores: the token secret
(mode 0600), the node's address and certificate fingerprint, generated configs, a throwaway RSA key that belongs to
no GitHub App, and an SSH `known_hosts` file. The token secret never appears on a command line or in output. Don't
commit any of it.
