# tine architecture

This is how tine works as of **v0.1.0**, the end of step 1: one service on a Linux server that creates microVMs, runs commands in them and destroys them, safely and without leaking anything.

Each section starts in plain words and then gives the detail. The *why* of the project is in [BRIEF.md](../BRIEF.md), and the step-by-step plan, including what comes after v0.1.0, is in [PLAN.md](PLAN.md).

## Contents

- [The big picture](#the-big-picture)
- [The code](#the-code)
- [Life of a VM](#life-of-a-vm)
- [Isolation: what keeps a VM in](#isolation-what-keeps-a-vm-in)
- [Networking](#networking)
- [Talking to the guest: vsock and tine-agent](#talking-to-the-guest-vsock-and-tine-agent)
- [Images and disks](#images-and-disks)
- [State, restarts and recovery](#state-restarts-and-recovery)
- [The API](#the-api)
- [Observability](#observability)
- [Running it: systemd](#running-it-systemd)
- [Testing](#testing)
- [Measured numbers](#measured-numbers)
- [Known limits](#known-limits)

## The big picture

**In plain words:** a program called `tined` runs on the server and owns every VM. You ask it for things with the `tine` command. For each VM, tined builds a locked box (its own files, its own user, its own slice of CPU and memory, its own network) and starts Firecracker inside it. Firecracker runs the guest: a real Linux kernel on virtual hardware. Inside every guest, a small program called `tine-agent` runs commands for tined. tined talks to it over vsock, a direct pipe between host and guest, so no SSH or guest network is involved.

```
 you
  │  tine create / exec / ls / destroy
  ▼
 tine (CLI) ──── HTTP over /run/tine/tine.sock ────┐
                                                   ▼
┌─────────────────────────────── tined ─────────────────────────────────────┐
│ api      HTTP routes, errors, request IDs, /metrics                       │
│ vm       create (with undo) · destroy · exec · reconcile · reaper         │
│ state    SQLite: VMs, images, every state change                          │
└──────┬──────────────────┬─────────────────────┬───────────────────────────┘
       │ netns            │ jailer               │ firecracker + agentproto
       ▼                  ▼                      ▼
 ┌───────────── for each VM ──────────────────────────────────────────────┐
 │ network namespace tine-<id>   tap0 ─ veth ─ host, nftables rules       │
 │ jail /srv/tine/jail/firecracker/<id>/root   own uid, own cgroup        │
 │   firecracker ── KVM ── guest Linux                                    │
 │                           └─ tine-agent (vsock port 1024)              │
 └────────────────────────────────────────────────────────────────────────┘
```

Some properties that hold throughout:

- **tined is the only thing that changes VMs.** The CLI is a thin client of tined's HTTP API, the same split as `docker` and `dockerd`.
- **VMs don't depend on tined staying up.** Each Firecracker runs in its own cgroup, outside tined's, so restarting or even killing tined leaves VMs running. When tined starts, it reconciles its records with what's actually on the host.
- **Every operation is timed.** Each create returns a `timings_ms` breakdown, and the same numbers feed Prometheus.

## The code

Three binaries, all Go, built on the Mac (or in CI) for linux/amd64 and run on the server:

| Binary | What it is |
|---|---|
| `cmd/tined` | The service. `tined serve` runs under systemd; `tined image import` builds images. |
| `cmd/tine` | The CLI: `create`, `ls`, `inspect`, `exec`, `destroy`, `images`, `version`. |
| `cmd/tine-agent` | Runs inside every guest and serves `/health` and `/exec` over vsock. |

The packages under `internal/`:

| Package | Job | Imports from tine |
|---|---|---|
| `api` | HTTP server and Go client for tined's API | `vm`, `state`, `image`, `agentproto`, `metrics`, `logging` |
| `vm` | The VM manager: lifecycle, rollback, reconcile, reaper | everything below |
| `state` | SQLite store, migrations and the VM state machine | none |
| `jailer` | Builds a VM's jail and the jailer command line | `storage` |
| `netns` | A VM's network namespace, devices and firewall rules | none |
| `firecracker` | Typed client for Firecracker's API socket | none |
| `agentproto` | The host ↔ guest protocol, and the host's client for it | none |
| `agent` | The server inside the guest | `agentproto` |
| `image` | Builds and reads images | none |
| `storage` | Reflink copies and hard links | none |
| `idalloc` | Allocates network slots | none |
| `metrics`, `logging` | Prometheus metrics; structured logs with request and VM IDs | none |
| `testenv` | Helpers that find test artifacts and skip tests that can't run | none |

`vm` is the one place that knows about everything. The packages below it each do one job and know nothing about VMs as a whole, so each can be tested on its own, most against fakes and all against the real thing on the server.

## Life of a VM

**In plain words:** a VM is a row in a database that moves through a few states. tined builds the VM one stage at a time, and if any stage fails it undoes the earlier ones in reverse order, so a failed create leaves nothing behind.

### States

The normal path is `creating → running → destroying → destroyed`. Anything can go wrong on the way, so `creating`, `running` and `destroying` can also go to `failed`, and from `failed` the only way on is `destroying`.

| From | Allowed to |
|---|---|
| `creating` | `running`, `failed`, `destroying` |
| `running` | `destroying`, `failed` |
| `failed` | `destroying` |
| `destroying` | `destroyed`, `failed` |

The store enforces this table, not the callers. Every change is recorded in `vm_events` with its reason, and a failed VM keeps its error, which `tine inspect` shows. Writes take SQLite's lock up front, so when two destroys race, exactly one wins the change to `destroying`.

### Create

`POST /v1/vms` runs these stages in order. The times are from 100 creates, one at a time, on the dev server ([measured numbers](#measured-numbers)).

| Stage | What happens | p50 |
|---|---|---|
| allocate | Check the image and the VM limit. Pick an ID (`vm-` plus 10 base32 characters), a network slot and a uid. Record the VM as `creating`. | 0.30 ms |
| network | Create the namespace `tine-<id>` with `tap0`, the veth pair, routes, NAT and firewall rules. | 5.2 ms |
| jail | Create the chroot, hard-link the kernel, and reflink-copy the 4 GiB disk. | 0.32 ms |
| start | Run the jailer, which sets up the cgroup and namespace and becomes Firecracker. | 0.20 ms |
| configure | Over Firecracker's API: logger, machine config, kernel and boot arguments, root drive, network interface, vsock, entropy device, then `InstanceStart`. | 21 ms |
| agent | The guest kernel and systemd boot until `tine-agent` answers `/health` over vsock. | 1.81 s |
| **total** | | **1.83 s** (p99 1.89 s) |

Almost all of a create is the guest booting. Step 2 replaces this cold boot with a restore from a ready snapshot.

**Undo.** Each stage that creates something pushes its undo onto a stack. If a later stage fails:

1. The VM is marked `failed`, with the stage and the error.
2. The undo stack runs in reverse: stop Firecracker, remove the jail, remove the network.
3. If all of that worked, the VM becomes `destroyed` and its slot is freed. If anything couldn't be removed, the VM stays `failed`, its slot stays taken, and the next destroy or restart tries again.

A destroy that arrives during a create cancels the create, waits for it to undo itself, and then finishes. Unit tests fail each stage in turn against a fake host and check the undo, and an integration test forces a real failure and checks the host is clean.

### Destroy

`DELETE /v1/vms/{id}` takes a per-VM lock, moves the VM to `destroying` and tears it down:

1. **Kill Firecracker** by writing to its cgroup's `cgroup.kill` and waiting until `cgroup.procs` is empty. This works whether or not this tined started the process, which matters after a restart.
2. **Remove the jail.** This is refused while the cgroup still exists, so files are never removed from under a live process.
3. **Remove the network namespace**, the veth and the VM's host rules.

Teardown stops at the first step that fails, and the VM is marked `failed` so a later destroy can carry on. Destroy is idempotent: destroying a destroyed VM returns it unchanged. p50: **32 ms**.

The guest isn't asked to shut down first. In task 1.4 the guest didn't stop within 5 s of Ctrl+Alt+Del, and a sandbox's disk is thrown away anyway.

### Exec

`POST /v1/vms/{id}/exec` takes a command (argv, env, working directory, stdin, a timeout and an output cap) and streams the result:

```
tine ──HTTP──► tined ──vsock──► tine-agent ──► the command (own process group)
     ◄─NDJSON──      ◄─NDJSON──
```

1. tined connects to the VM's vsock socket in the jail, writes `CONNECT 1024`, reads `OK <port>`, and speaks HTTP over that connection.
2. The agent starts the command in its own process group and streams output as it happens, one JSON object per line: `{"stream":"stdout","data":"<base64>"}`.
3. The last line is always the exit status: `{"exit":{"code":0,"duration_ms":0.96}}`. It also records a killing signal, a timeout, or that output passed the cap (10 MiB by default) and was cut.
4. A timeout, or the client hanging up (Ctrl+C on `tine exec`), kills the whole process group. A background process the command leaves running doesn't keep the exec waiting.

`tine exec` exits with the command's own code, 128+n for a signal, and 124 for a timeout. Running `true` takes **1.7 ms** round trip through the API, of which about 1 ms is inside the guest.

## Isolation: what keeps a VM in

**In plain words:** tine runs code nobody has checked, so each VM sits inside several separate walls. Breaking out of one still leaves the others.

| Layer | What it does |
|---|---|
| KVM | The guest runs its own kernel on virtual hardware. A guest exploit has to escape the hypervisor, not just a container. |
| Firecracker | A minimal virtual machine monitor: a few virtio devices and a serial console, with its built-in seccomp filters (tine doesn't turn them off). |
| Jail: chroot | Firecracker sees only its own jail: the kernel, its disk, two sockets and a log. No other VM's files. |
| Jail: uid | Each VM's Firecracker runs as its own unprivileged user, `200000 + slot`. The jail's files belong to it with mode 0700. |
| Jail: cgroup | `cpu.max` (the VM's vCPUs), `memory.max` (guest memory plus 256 MiB) and `pids.max` (128) for each Firecracker. |
| Network namespace | Each VM has its own network stack. Its only way out is the veth to the host, through the firewall below. |
| Firewall | VMs reach the internet, and nothing else: not each other, not the host, not private addresses, not the EC2 metadata service. |
| API socket | `/run/tine/tine.sock` is mode 0660, `root:tine`. Anyone else can't talk to tined at all. |

This is a real jail, from `sudo ls -la /srv/tine/jail/firecracker/<id>/root` on the dev server (trimmed, with notes added):

```
drwx------ 4 200106 200106        161 .
srwxr-xr-x 1 200106 200106          0 api.sock         Firecracker's API
drwx------ 3 200106 200106         62 dev              the device nodes Firecracker needs, such as /dev/kvm
-rwxr-xr-x 1 200106 200106    3676680 firecracker      copied in by the jailer
-rw------- 1 200106 200106       4712 firecracker.log  Firecracker's own log
-rw------- 1 200106 200106 4294967296 rootfs.ext4      reflink copy: shares the image's blocks
-rw-r--r-- 2 root   root     27940040 vmlinux          hard link to the image's kernel (read-only)
srwxr-xr-x 1 200106 200106          0 vsock.sock       host end of the guest's vsock
```

The isolation is checked from inside real VMs, by an e2e test that runs 10 at once. Each must reach `1.1.1.1:443`, and each must fail to reach the metadata service, the host's SSH port through its gateway, and another VM's network. Blocked traffic is dropped, not refused, so the test requires a *timeout*. Without the firewall, a connection to another VM's namespace address would be refused at once by the kernel. A timeout proves the firewall stopped it, rather than nothing listening.

## Networking

**In plain words:** every VM gets its own private network, and inside it every VM has *the same* address. A VM made by copying another (a fork, in step 3) wakes up with the same network settings, and they're already right. Two layers of address translation get each VM's traffic out to the internet.

```
 host (root namespace)                        namespace tine-<id>
 ┌──────────────────────────┐                 ┌──────────────────────────────────────┐
 │ uplink ── internet       │                 │ veth0  10.200.N.2/30                 │
 │   ▲ masquerade           │   veth pair     │   ▲ masquerade 172.16.0.2 → veth0    │
 │ tine<slot> 10.200.N.1/30 ┼─────────────────┼───┘                                  │
 └──────────────────────────┘                 │ tap0  172.16.0.1/30 ◄──► guest eth0  │
                                              │                          172.16.0.2  │
                                              └──────────────────────────────────────┘
```

- **Inside every namespace:** `tap0` is `172.16.0.1/30`, and the guest is `172.16.0.2` with MAC `06:00:AC:10:00:02`. The guest's address is set on the kernel command line (`ip=172.16.0.2::172.16.0.1:255.255.255.252::eth0:off`), so it's up before userspace starts.
- **Between namespace and host:** a veth pair. The VM's slot picks a /30 out of `10.200.0.0/16` (16,384 slots), and the host end is named `tine<slot>`.
- **Firewall and NAT** are nftables, all in one `inet tine` table on the host and one in each namespace. Rules are generated in Go as nft scripts and applied with `nft -f`, which applies a whole script or none of it. Cleanup is deleting the table or the namespace.

The host's rules, as `sudo nft list table inet tine` shows them:

```
chain input {
    iifname "tine*" ct state established,related accept
    iifname "tine*" drop comment "VMs can't reach the host's own services"
}
chain forward {
    iifname "tine*" meta nfproto ipv6 drop comment "no IPv6 for VMs"
    iifname "tine*" oifname "tine*" drop comment "no traffic between VMs"
    iifname "tine*" ip daddr { 0.0.0.0/8, 10.0.0.0/8, 100.64.0.0/10, 127.0.0.0/8, 169.254.0.0/16,
                               172.16.0.0/12, 192.168.0.0/16, 224.0.0.0/3 }
        drop comment "no private, link-local or metadata addresses"
    oifname "tine*" ct state established,related accept
    oifname "tine*" drop comment "nothing opens connections into VMs"
}
chain postrouting {
    ip saddr 10.200.0.0/16 oifname != "tine*" masquerade
}
```

And each namespace's (default drop on forward, so only the guest's own outbound traffic passes):

```
chain input {
    iifname "tap0" ct state established,related accept
    iifname "tap0" meta l4proto icmp accept
    iifname "tap0" drop comment "the guest can't reach anything in its namespace"
}
chain forward {                       # policy drop
    iifname "tap0" oifname "veth0" ip saddr 172.16.0.2 accept
    iifname "veth0" oifname "tap0" ct state established,related accept
}
chain postrouting {
    oifname "veth0" ip saddr 172.16.0.2 masquerade
}
```

One implementation detail: Linux creates a tap device in the network namespace of the *thread* that asks for it, so `netns` does that work on a goroutine locked to one OS thread that has switched into the VM's namespace, and switches back before releasing the thread.

## Talking to the guest: vsock and tine-agent

**In plain words:** vsock is a direct pipe between the host and the guest. It works even if the guest's network is broken or firewalled, and nothing on the network can reach it. tine-agent listens on it and runs commands for tined.

- Firecracker exposes the guest's vsock (CID 3) as a Unix socket in the jail, `vsock.sock`. A host connection writes `CONNECT 1024\n` and gets `OK <port>\n` back, and from then on it's a plain byte stream to the agent's port 1024.
- The agent serves HTTP/1.1 on it: `GET /health` and `POST /exec`. The request and response types are defined once in `agentproto`, which both sides import.
- tined dials a fresh connection for each request. There's nothing to reconnect after a snapshot, which in step 2 resets vsock connections.
- In the image, the agent's systemd unit starts early (`DefaultDependencies=no`, wanted by `sysinit.target`) and restarts forever. It only needs vsock, not the network or a login.
- Commands run as root in the guest, in `/root`, with output capped. The guest is the sandbox, so root inside it is fine; the walls are outside it.

This is how production sandboxes do it too, for example E2B's `envd`.

## Images and disks

**In plain words:** an image is a kernel and a disk that VMs boot from. Every VM gets its own copy of the disk, but copying is nearly free: the filesystem shares the blocks until the VM changes one.

`tined image import base` (or `make base-image`) builds the base image from Firecracker's CI Ubuntu 24.04 squashfs:

1. Unpack the squashfs into a directory (`unsquashfs`). Nothing is ever mounted.
2. Customize it:
   - Install `tine-agent` and its unit.
   - Mask what a sandbox doesn't need: the SSH server, the resolver service, wait-online and apt timers.
   - Write a static `/etc/resolv.conf`, the hostname, and the stamps that stop every boot from re-running `ldconfig`.
   - Never write through a symlink in the unpacked tree, which could point at the host's own files.
3. Pack it into a 4 GiB ext4 disk with `mkfs.ext4 -d`, check it with `e2fsck`, write `image.json`, and rename the finished directory into place. An import is atomic: a failed one leaves no half-image.

An import takes about 2.6 s. `image.json` records where every part came from, with checksums:

```json
{
  "format_version": 1,
  "name": "base",
  "kernel": { "source": "vmlinux-6.18.56", "sha256": "af34…" },
  "rootfs": { "size_bytes": 4294967296, "source": "ubuntu-24.04.squashfs", "source_sha256": "91da…" },
  "agent":  { "version": "…", "sha256": "4b58…" }
}
```

**Per-VM disks are reflinks.** `/srv/tine` is an XFS volume, where `FICLONE` copies a file by sharing its blocks. Copying a 2 GB disk took 2,725 ms normally and 5 ms as a reflink, using no extra space. `storage.Clone` never falls back to a real copy: on a filesystem without reflinks it fails, rather than quietly making creates seconds slower. The kernel is read-only, so it's hard-linked instead.

### Files on the server

```
/srv/tine/                               XFS data volume (50 GB gp3, reflink=1)
├── images/<name>/                       vmlinux, rootfs.ext4, image.json
└── jail/firecracker/<vm-id>/root/       one jail per VM (shown above)
/sys/fs/cgroup/tine/<vm-id>/             each VM's cgroup (tine/ is root-only, 0750)
/var/run/netns/tine-<vm-id>              each VM's network namespace
/var/lib/tine/tine.db                    SQLite state
/run/tine/tine.sock                      the API socket (root:tine, 0660)
```

## State, restarts and recovery

**In plain words:** tined writes down everything it does in a small database. If it crashes or restarts, it compares those notes with what's really running and fixes any mismatch before it accepts new work. VMs keep running while it's gone.

**The store** is SQLite (`modernc.org/sqlite`, pure Go, no CGO) in WAL mode:

- **Tables:** `images`, `vms`, and `vm_events`, a history of every state change with its reason.
- **Uniqueness:** a network slot and a uid can each belong to only one VM that isn't destroyed (partial unique indexes).
- **Migrations:** numbered SQL files embedded in the binary, each run in a transaction. tined refuses a database written by a newer version.
- **Times** are stored fixed-width (`2006-01-02T15:04:05.000000000Z`), because SQLite compares them as text, and RFC 3339's variable-length fractions sort wrongly.

**VMs survive tined.** The jailer moves each Firecracker into its own cgroup under `/sys/fs/cgroup/tine`, outside tined's service cgroup. systemd's `KillMode=mixed` signals only tined itself, so stopping, restarting or crashing tined leaves VMs running. A restart took 41 ms with VMs still up, and after a `SIGKILL` systemd had tined back in 2.2 s (its `RestartSec` is 2 s).

**Reconcile** runs at startup, before the API socket opens:

| Found | Meaning | What tined does |
|---|---|---|
| `creating` | tined stopped mid-create | Undo whatever exists; the record says why |
| `destroying` | tined stopped mid-destroy | Finish the destroy |
| `running`, Firecracker alive | Normal | Keep it |
| `running`, Firecracker gone | It died while tined was down | Clean it up as failed |
| `failed` | Kept for inspection | Keep it until it's destroyed or expires |
| A namespace, jail, cgroup or veth no live VM owns | Leftover | Remove it, killing any processes in the cgroup |

It logs a one-line report, for example `kept 1; cleaned 1; orphans removed 2; errors 0`. While tined runs, it watches every Firecracker it started, and one that exits on its own marks its VM `failed`.

**The reaper** wakes every 15 s and destroys VMs past their time to live (1 hour by default), so forgotten sandboxes clean themselves up. Hourly, it also purges records destroyed more than 7 days ago.

## The API

**In plain words:** tined's API is ordinary HTTP and JSON, served on a Unix socket instead of a network port. Anything that can speak HTTP can drive tine, `curl` included.

| Method and path | What it does |
|---|---|
| `POST /v1/vms` | Create a VM: `{"image", "vcpus", "memory_mib", "ttl_seconds"}`, each optional |
| `GET /v1/vms` | List VMs (`?all=true` includes destroyed ones) |
| `GET /v1/vms/{id}` | One VM, with its timings |
| `POST /v1/vms/{id}/exec` | Run a command; the response streams NDJSON frames |
| `DELETE /v1/vms/{id}` | Destroy a VM (idempotent) |
| `GET /v1/images` | List images |
| `GET /healthz` | tined's version, uptime and VM count |
| `GET /metrics` | Prometheus metrics |

```bash
curl --unix-socket /run/tine/tine.sock -d '{"vcpus":1,"memory_mib":512}' http://tined/v1/vms
```

Unknown JSON fields are rejected, so a typo like `"cpus"` fails loudly instead of being ignored. Errors look like `{"error":{"code":"capacity","message":"…"}}`:

| Status | Code | When |
|---|---|---|
| 400 | `invalid_request` | Bad JSON, an unknown field, an impossible size |
| 404 | `not_found`, `image_not_found` | No such VM, image or endpoint |
| 409 | `not_running`, `conflict` | Exec in a VM that isn't running; a change the state machine refuses |
| 503 | `capacity` | The VM limit (64 by default) or every network slot is taken |
| 500 | `internal` | Anything else; tined logs it with the request ID |

Every response carries an `X-Request-Id` header, which is also on every log line written while handling that request.

## Observability

- **Timings in every response.** A create's `timings_ms` gives each stage above, and a destroy adds `destroy`.
- **Prometheus metrics** on the socket at `/metrics`, and on `127.0.0.1:9101` for a Prometheus server:
  - `tine_vm_create_stage_seconds{stage}`, `tine_vm_creates_total{result}`, `tine_vm_create_failures_total{stage}`
  - `tine_vm_destroy_seconds`, `tine_vm_destroys_total{result}`
  - `tine_exec_seconds`, `tine_execs_total{result}`
  - `tine_firecracker_api_seconds{endpoint}`, `tine_firecracker_api_errors_total{endpoint}`
  - `tine_api_request_seconds{method,route}`, `tine_api_requests_total{method,route,code}`
  - `tine_vms{state}`, `tine_reaper_destroyed_total`, `tine_reconcile_actions_total{action}`, `tine_build_info{version}`

  Labels are routes like `/v1/vms/{id}`, never VM IDs, so the number of series stays fixed however many VMs come and go.
- **Logs** are JSON through `log/slog` to journald (`journalctl -u tined`). The request ID and the VM ID travel in the request's context, so every line written while handling a request carries both, wherever in the code it's written.
- **The guest's serial console** goes to `console.log` in the VM's jail directory, beside `root/` so the jailed Firecracker can't reopen it, and to a file rather than a pipe so it doesn't depend on tined staying up. Firecracker's own log is `firecracker.log` inside the root. A VM in the `failed` state keeps its jail until it's destroyed, so a boot failure can be read after the fact.

## Running it: systemd

tined runs as `deploy/tined.service`, which `make deploy` installs:

| Setting | Why |
|---|---|
| `Type=notify` | tined tells systemd it's ready only after reconcile has finished and the socket is open |
| `KillMode=mixed` | Stopping tined signals tined only. VMs are in their own cgroups and keep running. |
| `Restart=on-failure`, `RestartSec=2s` | A crashed tined comes back by itself, and reconciles when it does |
| `TimeoutStopSec=40s` | In-flight requests get 30 s to finish on `SIGTERM` |
| `RequiresMountsFor=/srv/tine` | tined never starts without its data volume |
| `RuntimeDirectory=tine`, `StateDirectory=tine` | `/run/tine` for the socket, `/var/lib/tine` (0700) for the database |
| `LimitNOFILE=1048576` | Each VM holds a few file descriptors |

## Testing

**In plain words:** the fast tests run anywhere in seconds, using fakes. The slow tests run on the real server, against real Firecracker and real VMs, and the last of them checks that nothing at all is left behind.

| Level | Command | Runs where | What it covers |
|---|---|---|---|
| Unit | `make check` (also CI on every push) | Mac, CI | Every package, with the race detector. Fakes: a fake Firecracker socket, a fake host that fails each create stage in turn, the real agent and client over Unix sockets. |
| Integration | `make test-integration` | Dev server, as root | Each low-level package against the real thing: Firecracker, the jailer, namespaces and nftables, reflinks, image import, and the VM manager booting real VMs. |
| End to end | `make e2e` | Dev server, real tined under systemd | Black-box through the API (list below), ending in a strict leak check |
| Measure | `make measure` | Dev server | The numbers below |

The e2e suite:

- 50 creates, execs and destroys, 5 at a time
- 20 execs at once in one VM
- destroy during create
- `SIGKILL` to tined mid-create, and again with VMs running (they survive with their data)
- a VM whose Firecracker is killed while tined is down
- 10 VMs at once, checking isolation as described above
- a VM with a 60 s time to live, which must be reaped while the rest run

At the end, the leak check requires zero live VMs, network namespaces, veths, jails, cgroups and Firecracker processes. The suite takes about 2 minutes.

Bugs these tests caught before release:

- **A destroy after a tined restart left Firecracker running.** The old PID check looked for `--id=<id>`, but the jailer passes `--id <id>`, and cleanup carried on regardless. Found by the restart test. Fixed by killing through the VM's cgroup, stopping teardown at the first failure, and refusing to remove a jail whose cgroup still exists.
- **Ten racing destroys all "won".** The store accepted a change from a state to itself. Found by the store's concurrency test. Fixed by refusing same-state changes.
- **Times sorted wrongly** as text, which would have had the reaper compare expiry times out of order. Fixed by storing them fixed-width.
- **Agent tests hung in CI.** `/root` exists on Linux runners but isn't enterable by a non-root user. Fixed in the agent, which now picks a working directory it can use, and the tests got timeouts.

## Measured numbers

From `make measure` and `make e2e` on the dev server: an AWS m8i.xlarge (4 vCPUs, 16 GiB) in ap-south-1, running the VMs through **nested virtualization**, which adds overhead. Bare-metal numbers come in step 4. The setup was Firecracker v1.17.0, guest kernel 6.18.56, and the Ubuntu 24.04 base image, with VMs at the default 2 vCPUs and 1 GiB unless noted.

| What | Result |
|---|---|
| Cold-boot create, one at a time (n=100) | p50 **1.83 s**, p99 1.89 s, max 1.90 s |
| … of which tine's own work (allocate to configure) | about **27 ms**; the rest is the guest booting |
| Create with 5 at once (n=50, e2e) | p50 2.8 s, p99 3.2 s |
| Destroy (n=100) | p50 **32 ms**, p99 42 ms |
| Exec `true` through the API (n=200) | p50 **1.7 ms**, p99 3.3 ms; about 1 ms of it inside the guest |
| Exec `true` with the `tine` CLI (n=50) | about 6 ms, including starting the CLI |
| Host memory per idle VM (n=5, 30 s after boot) | Firecracker RSS **127 MiB** (124 MiB of it guest memory); 168 MiB charged to its cgroup, including page cache |
| tined restart with VMs running | 41 ms; the VMs keep running |
| tined back after `SIGKILL` | 2.2 s (systemd waits 2 s before restarting it) |
| Image import (4 GiB disk) | about 2.6 s |
| Reflink copy of a 2 GB disk | 5 ms (2,725 ms as a normal copy) |

A guest with 1 GiB uses about 127 MiB on the host when idle, because memory is only handed out when the guest touches it. That's what makes many VMs per host affordable.

## Known limits

These are known and deliberate for v0.1.0. Most are what the next steps are for.

- **Creates are cold boots, 1.8 s each.** Step 2 restores new VMs from a ready snapshot instead.
- **One host only.** Sharing memory and disk between forks only works on one machine, and that's the point of step 3.
- **A VM that dies after a tined restart isn't noticed straight away.** tined watches the Firecracker processes it started. One started by an earlier tined is only checked at the next restart, or when something touches it: an exec then fails, and a destroy cleans it up. A periodic liveness check would close this.
- **No IPv6 for VMs**, and outbound traffic is otherwise unrestricted (no egress allow-list or rate limits yet).
- **Access is all or nothing.** Anyone in the `tine` group can create, use and destroy every VM. There are no per-user quotas or ownership.
- **Destroy kills the guest** without asking it to shut down. The disk is thrown away, so nothing is lost that matters.
- **The base image's disk is a fixed 4 GiB**, and there's one image so far.
