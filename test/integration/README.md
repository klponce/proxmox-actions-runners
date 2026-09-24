# Integration suite

Runs the project against a real Proxmox VE 9 node. All it needs is root SSH access to a **throwaway** node: it
creates its own test objects, runs the Go integration tests and `parcon check proxmox`, and can remove everything
again.

GitHub isn't covered yet; the controller test uses the in-memory GitHub fake.

## The node

- Proxmox VE 9.x, standalone (not in a cluster), with `/dev/kvm`. A nested node needs its CPU type set to `host`
  on the outer host.
- A bridge with DHCP and internet access (`vmbr0` by default), used only to build the test template.
- About 12 GiB of free space on the VM storage and 2 GiB of free memory.
- Nothing you care about in the suite's VMIDs: 9000 and 9900–9910 by default. Setup refuses VMIDs used by
  anything outside its own pool.

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
| `test` | Runs the Go integration tests, `parcon check proxmox`, and the permission checks below |
| `teardown` | Removes everything `setup` created and restores the storage setting it changed |
| `all` | `setup`, then `test` (the default) |

`run.sh -h` lists the settings, such as `PAR_IT_STORAGE`, `PAR_IT_TEMPLATE_VMID`, and `PAR_IT_TEST_VMID`.

## What setup creates

| Object | Name |
| ------ | ---- |
| SDN simple zone and VNet | `parit`, `paritnet` |
| Pool | `par-it` |
| Role | `PARIntegration`, with exactly the privileges in `internal/proxmox/access.go` |
| User and privilege-separated API token | `par-it@pve!it` |
| ACLs for the user and token | the pool, the storage, and the VNet |
| Test template | VMID 9000: Ubuntu 26.04 with the QEMU guest agent, `/run/par-runner` created at boot, tagged like a runner template |

The names differ from a real install's (`parzone`, `parnet`, `par-runners`), so the suite can't touch one.

The template is a test fixture, not how the product builds templates: it installs the guest agent with a cloud-init
snippet, which needs the `snippets` content type on the `local` storage. Teardown restores that setting.

## What test checks

1. `internal/proxmox` integration tests: version, permissions, VM listing (including that the token sees the
   template's pool), storage, and a clone → configure → grow disk → start → guest agent write and exec → destroy
   lifecycle.
2. `internal/controller` integration test: the real controller creates a worker and delivers its JIT config into the
   guest, adopts it after a restart, retires it when it powers off, and retires an idle worker on scale-down.
3. `parcon check proxmox` passes.
4. `parcon check proxmox` fails without `Pool.Audit`, because the token then can't see which pool a VM is in.
5. `parcon check proxmox` fails when only the token, not its user, has the VNet ACL, because a privilege-separated
   token gets only what both have.

Steps 4 and 5 change the test role on purpose and put it back afterwards, even if a check fails.

## Private data

Everything the suite writes locally goes to `.agents/integration/` (mode 0700), which git ignores: the token secret
(mode 0600), the node's address and certificate fingerprint, a generated config, and an SSH `known_hosts` file. The
token secret never appears on a command line or in output. Don't commit any of it.
