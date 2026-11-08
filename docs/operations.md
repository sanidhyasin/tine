# Running tine

How to install tined on a host, keep it running, watch it, and find out what went wrong. How it works is in [architecture.md](architecture.md); the API in [api.md](api.md).

## The host

| Needs | Why |
|---|---|
| Linux on x86-64 with KVM (`/dev/kvm`): bare metal, or a VM with nested virtualization | Firecracker. Nested virtualization works, but slower, and runs out of guest entries at about 60 vCPUs (see [results.md](results.md)). |
| An XFS data volume at `/srv/tine`, with reflinks | Disks and snapshots are reflink copies: instant, and sharing blocks. |
| Kernel 6.7 or later | `--ksm` (earlier kernels drop the setting at exec); everything else runs on older ones. |
| cgroup v2 | Each VM's limits. |
| `vm.dirty_ratio` of 60 | Snapshots of big VMs otherwise pause them for seconds (see [architecture.md](architecture.md#taking-a-snapshot)). |
| Docker, configured to leave the firewall alone | `tined image build` only. |

`scripts/host-setup.sh` sets all of it up on Ubuntu 24.04 and downloads Firecracker, a guest kernel and an Ubuntu root filesystem. It's safe to run again. On the AWS dev server, `./scripts/dev-server.sh setup` runs it.

## Installing and upgrading

From a checkout, on your machine:

```bash
make deploy        # build, copy to the server, install the binaries and tined.service, restart tined
make base-image    # build the "base" image and warm its template
```

Restarting tined leaves running VMs alone: each lives in its own cgroup, and tined reconciles its records with them when it comes back. An upgrade of tined is a restart.

**An upgrade of Firecracker or the host kernel** makes every snapshot unrestorable (Firecracker only restores snapshots taken by the same version, on the same CPU and kernel). tined warms the images in `--warm-images` again by itself, and refuses creates from other snapshots with `incompatible`. VMs' own snapshots can't be saved; they're collected once their VMs are gone.

**An upgrade of tine-agent** needs the images built again (`tined image import ... --force`, or `make base-image`). An image whose agent is too old for a feature, such as file transfers, says so.

## Configuration

`tined serve` takes its settings as flags; `deploy/tined.service` passes the defaults. To change one on a host, add a drop-in rather than editing the unit:

```bash
sudo systemctl edit tined   # then, in the editor:
# [Service]
# ExecStart=
# ExecStart=/usr/local/bin/tined serve --metrics-listen 127.0.0.1:9101 --overcommit 2 --max-vms 200
```

The ones worth knowing:

| Flag | Default | |
|---|---|---|
| `--overcommit` | 4 | Guest memory of all VMs together, as a multiple of the host's memory. |
| `--cpu-overcommit` | 8 | vCPUs of all VMs together, per host CPU. On nested virtualization, keep the total under about 60. |
| `--min-free-memory-mib` | 1024 | Host memory new VMs must leave available. |
| `--max-vms` | 512 | VMs at once. |
| `--max-fork-children`, `--fork-parallelism` | 100, the CPU count | Children of one fork, and how many are restored at once. |
| `--default-ttl` | 1h | How long VMs live when a request doesn't say. The reaper destroys expired VMs every 15 s. |
| `--warm-images` | `base` | Images tined keeps a template of, at the default size. |
| `--max-snapshots-per-vm`, `--min-free-mib` | 16, 2048 | Snapshots per VM, and disk a snapshot must leave free. |
| `--diff-snapshots`, `--balloon`, `--ksm`, `--page-server-huge-pages` | off | Optional behaviours, measured in [results.md](results.md). |

## Watching it

- **Is it up?** `tine version` shows both ends; `curl --unix-socket /run/tine/tine.sock http://tined/healthz`.
- **Metrics:** Prometheus on `127.0.0.1:9101/metrics` (and on the socket). The ones to alert on:
  - `tine_vm_creates_total{result="failed"}` and `tine_vm_create_failures_total{stage}`: creates failing, and where.
  - `tine_snapshot_stage_seconds{stage="paused"}`: how long snapshots freeze VMs.
  - `tine_api_requests_total{code=~"5.."}`: refusals for capacity (503) and failures (500).
  - `tine_vms{state="failed"}`: VMs that died.
- **Logs:** `journalctl -u tined`, as JSON. Every line written while handling a request has its `request_id` and `vm`, so one VM's story is `journalctl -u tined | grep vm-yxr4tliv4w`.
- **A VM's console:** `/srv/tine/jail/firecracker/<id>/console.log`, kept until the VM is destroyed, so a boot that failed can be read after.
- **What's running:** `tine ls --tree` shows every VM, the forks under each, and their memory.

## State and backups

| Where | What | Back it up? |
|---|---|---|
| `/var/lib/tine/tine.db` | SQLite: VMs, images, snapshots, every state change | Only if VM history matters. tined rebuilds what it needs from the host. |
| `/srv/tine/images/` | Images: kernel, disk, metadata | They can be built again. |
| `/srv/tine/snapshots/` | Snapshots and templates | No: they only restore on this host, with this Firecracker and kernel. |
| `/srv/tine/jail/` | Each running VM's jail: its disk, sockets, console | No: it goes with the VM. |

## When something goes wrong

| Symptom | Look at |
|---|---|
| `tine` says tined isn't running | `systemctl status tined`, `journalctl -u tined -n 50`. tined won't start without its data volume (`RequiresMountsFor=/srv/tine`). |
| `permission denied` on the socket | Your user needs the `tine` group: `sudo usermod -aG tine $USER`, then log in again. |
| Creates refused with `no_memory`, `no_cpu` or `capacity` | Admission control: the message has the numbers. Destroy VMs, or raise `--overcommit`, `--cpu-overcommit` or `--max-vms` if the host really has room. |
| Creates boot instead of restoring (slow) | `tine images`: the image's template is missing or out of date. tined warms `--warm-images` by itself; others need `tine image warm IMAGE`. |
| A VM is `failed` | `tine inspect VM` has the reason; its console log has the guest's side. |
| Guests fail to start beyond about 60 on a nested host | EC2's nested virtualization refusing guest entries (`KVM_EXIT_FAIL_ENTRY`): lower `--cpu-overcommit`. |
| Snapshots pause VMs for seconds | `vm.dirty_ratio` too low (tined warns at startup), or a slow disk: the VM's previous snapshot is still being written. |
| A fork's children run slowly at first | Expected: each first write to a page copies it (about 60 µs on the nested dev server, 8 µs on bare metal). `--strategy uffd-prefetch` trades memory for fewer faults. |

After a crash, tined reconciles on start: VMs whose Firecracker died are marked failed and cleaned up, half-made ones are undone, and anything on the host it has no record of (namespaces, jails, cgroups) is removed.

## The dev server

`scripts/dev-server.sh` manages the AWS dev server, in the AWS profile named by `TINE_AWS_PROFILE`:

```bash
./scripts/dev-server.sh up        # create or start it (under a minute)
./scripts/dev-server.sh status    # what's running, and what it costs
./scripts/dev-server.sh ssh       # log in
./scripts/dev-server.sh stop      # stop it: no compute charges while stopped
```

It stops itself after 60 minutes with no SSH connections and no running VMs, but stop it whenever you're done.
