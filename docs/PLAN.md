# tine: build plan

This is the plan for building tine from here to the finished project: a sandbox system that forks running microVMs in milliseconds, an AI agent demo that uses it, and a writeup with honest numbers.

Every step starts with a plain-words summary of what it builds and why, then has design notes, a task list where each task is about one commit, and a "done when" checklist.

The *why* behind the project is in [BRIEF.md](../BRIEF.md). This file is the *how*.

## Contents

- [Where we are](#where-we-are)
- [The finished system](#the-finished-system)
- [Design decisions](#design-decisions)
- [How we work](#how-we-work)
- [Step 1: Foundations](#step-1-foundations)
- [Step 2: Snapshots, templates and rollback](#step-2-snapshots-templates-and-rollback)
- [Step 3: Fork](#step-3-fork)
- [Step 4: Measure, then make it fast](#step-4-measure-then-make-it-fast)
- [Step 5: Python SDK, the AI agent demo, and the writeup](#step-5-python-sdk-the-ai-agent-demo-and-the-writeup)
- [Risks and unknowns](#risks-and-unknowns)
- [Glossary](#glossary)

## Where we are

**Step 0 is done.** You booted a microVM by hand on the dev server, logged in over SSH, watched memory being handed out lazily, and paused and resumed a running VM.

| Step | What you'll have at the end | Release |
|---|---|---|
| 1. Foundations | `tine create`, `exec`, `ls` and `destroy`, run by a service on the server | v0.1.0 |
| 2. Snapshots | Save points, rollback, and fast starts from pre-booted templates | v0.2.0 |
| 3. Fork | `tine fork <vm> -n 10`: ten running copies that share memory | v0.3.0 |
| 4. Measure and optimize | A benchmark harness, the improvement ladder, and published results | v0.4.0 |
| 5. SDK, demo, writeup | A Python SDK, the AI agent demo, a polished README, and the blog post | v1.0.0 |

## The finished system

```
YOUR MAC                                   THE SERVER (Linux + KVM)
┌───────────────────────┐                  ┌──────────────────────────────────────────────────────┐
│ tine CLI              │                  │ tined  (the tine service, runs as root)              │
│ Python SDK            │ ──── ssh ──────► │   API on /run/tine/tine.sock                         │
│ AI agent demo         │                  │   ├─ VM lifecycle: create / snapshot / fork / destroy│
└───────────────────────┘                  │   ├─ state in SQLite, metrics, logs                  │
                                           │   └─ for each VM:                                    │
                                           │        jailer → firecracker → microVM                │
                                           │        own network namespace (tap, veth, NAT)        │
                                           │        own copy-on-write disk                        │
                                           │        tine-agent inside the guest (talks over vsock)│
                                           │                                                      │
                                           │ /srv/tine  (XFS volume: images, VM disks, snapshots) │
                                           └──────────────────────────────────────────────────────┘
```

| Part | What it is | Where it runs |
|---|---|---|
| `tined` | The service that owns every VM. Everything goes through its API. | Server, as a systemd service |
| `tine` | The command-line tool. Sends requests to `tined`. | Server, and later your Mac |
| `tine-agent` | A small program inside every VM. Runs commands and handles files for `tined`. | Inside each guest |
| Python SDK | A Python wrapper around the API, for agents and scripts | Anywhere |
| `tine-bench` | Runs the benchmarks and records results | Server (step 4) |
| `tine-uffd` | Our own memory loader for restored VMs | Server (step 4) |

### Repository layout

```
tine/
├── cmd/
│   ├── tine/            CLI
│   ├── tined/           the service
│   ├── tine-agent/      runs inside every VM
│   ├── tine-bench/      benchmark driver (step 4)
│   └── tine-uffd/       userfaultfd page-fault handler (step 4)
├── internal/
│   ├── api/             API types, HTTP handlers, Go client
│   ├── firecracker/     client for Firecracker's API socket
│   ├── jailer/          builds each VM's jail (chroot, user, cgroups)
│   ├── netns/           network namespaces, tap, veth, NAT and firewall rules
│   ├── storage/         reflink copies, image store, snapshot store
│   ├── state/           SQLite state and migrations
│   ├── vm/              VM lifecycle: create, snapshot, restore, fork, destroy
│   └── agentproto/      host ↔ guest protocol, shared by tined and tine-agent
├── images/              image definitions (base, python)
├── deploy/              systemd unit and default config
├── sdk/python/          Python SDK (step 5)
├── examples/best-of-n/  the AI agent demo (step 5)
├── bench/               analysis scripts and published results (step 4)
├── scripts/             dev server and host setup
└── docs/
```

## Design decisions

These are settled unless we hit a real reason to change them. If we do, we record the change here.

1. **One server, one service.** All VMs run on one host, managed by one long-running service (`tined`). The CLI and SDK are thin clients. This is the same split as `dockerd` and `docker`. Sharing memory and disk between forks only works on one machine, so multi-host is out of scope.
2. **Go for everything on the host and in the guest.** It gives static binaries, easy cross-compiling from the Mac, and good libraries for Linux networking. Python is only used for the SDK, benchmark analysis and the demo.
3. **Our own small Firecracker client**, rather than the official Go SDK. We use about 12 endpoints, and owning that code makes timing and snapshot behavior easy to control.
4. **Every VM runs inside Firecracker's jailer.** The jailer gives each VM its own chroot, an unprivileged user and cgroup limits. It also solves a fork problem: restoring a snapshot needs every file at *the same path* as when it was taken. Inside a jail, every clone sees `/rootfs.ext4` and `/vsock.sock`, but each one points to its own file on the host. (Firecracker's `network_overrides` and `vsock_override` exist too, but they don't cover disks.)
5. **Every VM gets its own network namespace with identical addresses inside it.** The guest is always `172.16.0.2` on a tap device called `tap0`. A veth pair with a unique address per VM connects each namespace to the host, with NAT in between. Clones restored from one snapshot then need no network changes inside the guest at all.
6. **The host talks to the guest over vsock, not SSH.** vsock is a direct host-to-guest socket that doesn't depend on the guest's network. `tine-agent` serves a small HTTP API over it. This is how production systems do it (E2B's envd, DigitalOcean's agent).
7. **Copy-on-write disks via reflinks on XFS.** `/srv/tine` lives on an XFS volume, where copying a 2 GB disk image takes milliseconds because no data is copied until something changes. dm-thin is a possible comparison in step 4.
8. **Snapshots are immutable and shared.** A snapshot's memory file is never changed once a VM runs from it. Firecracker requires this. Children use hard links to the same memory file, so the host keeps one copy in its page cache.
9. **New VMs start from templates, not cold boots** (from step 2). Each image is booted once and snapshotted when it's ready. `tine create` restores from that snapshot, so every VM is file-backed from the start, which later lets fork save only the memory that changed.
10. **Every operation reports where its time went.** API responses include a `timings_ms` breakdown, such as `{"jail": 2.1, "network": 3.4, "restore": 11.8, "agent_ready": 4.0}`. The same numbers feed Prometheus histograms and the benchmarks. Measuring is built in, not added later.
11. **State lives in SQLite**, using the pure-Go `modernc.org/sqlite` so there's no CGO. When `tined` restarts, it reconciles its state with what actually exists: Firecracker processes, namespaces, jails and files.
12. **Firewalling uses nftables, in one `inet tine` table, written as plain nftables scripts.** Ubuntu 24.04 uses nftables. Keeping every tine rule in one table makes cleanup a single command and avoids touching other rules. The rules are text applied with `nft -f`, which is atomic (all of a script applies, or none of it) and readable: `nft list table inet tine` shows exactly what tine wrote. Kubernetes' nftables mode works the same way.

### Libraries

| Need | Library |
|---|---|
| CLI | `github.com/spf13/cobra` |
| Network devices and namespaces | `github.com/vishvananda/netlink`, `github.com/vishvananda/netns` |
| Firewall and NAT | the `nft` command, with scripts generated in Go |
| vsock in the guest | `github.com/mdlayher/vsock` |
| SQLite | `modernc.org/sqlite` |
| Metrics | `github.com/prometheus/client_golang` |
| Logging | `log/slog` (standard library), JSON to journald |
| Tests | standard `testing`, plus `github.com/google/go-cmp` |

Versions are pinned: Go 1.26, Firecracker v1.17.0, Ubuntu 24.04 on the host, and the guest kernel from Firecracker's CI (6.18).

## How we work

### The loop for every task

1. Pick the next unchecked task in this file.
2. Implement it, with its tests.
3. Read the diff, run `make check`, and run the end-to-end tests on the server when the task touches VMs.
4. Tick the task's box in this file, and commit both together as one small commit.

### Definition of done for every task

- [ ] Code plus tests. Unit tests run anywhere; anything touching KVM, networking or jails also gets an end-to-end test that runs on the server.
- [ ] `make check` passes: `gofmt`, `go vet`, `golangci-lint` and unit tests.
- [ ] No leaks: after the e2e tests, there are no stray Firecracker processes, namespaces, veth or tap devices, jails, mounts or nftables rules.
- [ ] Errors are wrapped with context (`fmt.Errorf("create jail %s: %w", id, err)`), every external call has a timeout, and nothing panics in normal operation.
- [ ] Every resource has a cleanup path, `destroy` is idempotent, and a half-finished `create` cleans up after itself.
- [ ] Docs are updated when behavior changes.
- [ ] One small, signed commit.

### Build and deploy

The Mac builds everything. The server only runs it.

| Command | What it does |
|---|---|
| `make build` | Builds `tine`, `tined` and `tine-agent` for linux/amd64 (`CGO_ENABLED=0`) |
| `make check` | Formatting, vet, lint and unit tests |
| `make deploy` | Builds, syncs to the server, installs, and restarts `tined` |
| `make e2e` | Runs the end-to-end tests on the server, including the leak check |

CI (GitHub Actions) runs `make check` and `make build` on every push. The end-to-end tests run on the dev server.

### Cost rules

- Stop the dev server whenever work pauses: `./scripts/dev-server.sh stop`.
- The paid items in this plan are the data volume in step 1 and the bare-metal run in step 4. Each one gets its cost stated when it's added, and nothing else is spent on.
- No waste: right-size things, delete what's no longer needed, and check `./scripts/dev-server.sh status` at the end of every session.

---

## Step 1: Foundations

**In plain words:** turn what you typed in terminal 2 during step 0 into real software. A service on the server (`tined`) owns all the VMs. The `tine` command asks it to do things. A small helper inside each VM (`tine-agent`) runs commands for us, so we no longer need SSH.

**What you'll have at the end:**

```
$ tine create
vm-7f3k2q9d1a  running  (cold boot in 1.2s: jail 2ms · network 4ms · boot 1.1s · agent 90ms)

$ tine exec vm-7f3k2q9d1a -- uname -r
6.18.56+

$ tine ls
ID              STATE    IMAGE  VCPUS  MEMORY  AGE
vm-7f3k2q9d1a   running  base   2      1 GiB   12s

$ tine destroy vm-7f3k2q9d1a
destroyed vm-7f3k2q9d1a (14ms)
```

The numbers above are made up; measuring the real ones is part of the step.

### Design

**Files on the server**

```
/srv/tine/                                 XFS data volume (reflinks work here)
├── images/<name>/                         vmlinux, rootfs.ext4, image.json
├── jail/firecracker/<vm-id>/root/         one jail per VM, created by the jailer
│   ├── vmlinux                            hard link to the image's kernel
│   ├── rootfs.ext4                        reflink copy of the image's disk
│   ├── api.sock                           Firecracker's API socket
│   └── vsock.sock                         host end of the vsock device
└── snapshots/                             (step 2)
/var/lib/tine/tine.db                      SQLite state
/run/tine/tine.sock                        tined's API socket (group: tine)
```

**Creating a VM (cold boot)**

1. Allocate an ID (`vm-` plus 10 lowercase base32 characters) and a network slot.
2. Network: create namespace `tine-<id>` containing `tap0` (`172.16.0.1/30`), plus a veth pair. The host end gets `10.200.<slot>.1/30` and the namespace end `.2/30`. Then add NAT inside the namespace and on the host.
3. Jail: create the chroot, hard-link the kernel, and reflink the disk.
4. Start `jailer --id <id> --exec-file firecracker --uid <uid> --gid <gid> --chroot-base-dir /srv/tine/jail --netns /var/run/netns/tine-<id> --cgroup-version 2 -- --api-sock /api.sock`.
5. Configure it over the API socket, exactly like step 0, but as Go code:
   - machine config (2 vCPUs, 1 GiB by default)
   - boot source, with the guest network configured on the kernel command line: `ip=172.16.0.2::172.16.0.1:255.255.255.252::eth0:off`
   - root drive
   - network interface on `tap0`
   - vsock (`guest_cid: 3`, `uds_path: /vsock.sock`)
   - entropy device (virtio-rng)
6. Send `InstanceStart`, then wait until `tine-agent` answers its health check.
7. Save the VM to SQLite and return it with `timings_ms`.

If any step fails, everything done so far is undone, in reverse order.

**Networking for each VM**

```
 host (root namespace)                        namespace tine-<id>
 ┌──────────────────────────┐                 ┌──────────────────────────────────────┐
 │ eth0 ── internet         │                 │ veth  10.200.N.2/30                  │
 │   ▲ NAT 10.200.0.0/16    │   veth pair     │   ▲ NAT 172.16.0.2 → 10.200.N.2      │
 │ veth  10.200.N.1/30  ────┼─────────────────┼───┘                                  │
 └──────────────────────────┘                 │ tap0  172.16.0.1/30 ◄──► guest eth0  │
                                              │                          172.16.0.2  │
                                              └──────────────────────────────────────┘
```

Firewall rules, all in the `inet tine` table:

- VMs can reach the internet.
- VMs **cannot** reach each other, the host's own services, or `169.254.169.254` (the EC2 metadata service, which could leak instance credentials).

**Host ↔ guest (vsock)**

- `tine-agent` listens on vsock port 1024 and serves HTTP/1.1.
- `tined` connects to `<jail>/vsock.sock`, writes `CONNECT 1024\n`, reads back `OK <port>\n`, and then speaks HTTP over that connection.
- Agent endpoints for step 1:
  - `GET /health`
  - `POST /exec`: takes argv, env, a working directory, stdin, a timeout and an output cap. It streams NDJSON frames (`{"stream":"stdout","data":"<base64>"}`) while the command runs, and always ends with an exit frame: `{"exit":{"code":0,"duration_ms":12.3}}`, which also reports a killing signal, a timeout or truncated output. Output is capped, by default at 10 MB.
  - A timeout, or tined hanging up, kills the command and everything it started (its process group). A background process the command leaves running doesn't keep `exec` waiting.
- The `agentproto` package defines these types once, for both sides, and holds the host's client, including the vsock `CONNECT` handshake.

**The base image**

`tined image import base` builds it. It's an admin command of `tined` rather than `tine`, because it needs root and host paths, and `tine` only ever talks to tined's API. `make base-image` runs it on the dev server.

- It starts from Firecracker's CI Ubuntu squashfs, which `host-setup.sh` keeps, unpacks it into a directory, and packs the result with `mkfs.ext4 -d`. Nothing is mounted.
- It adds `tine-agent` with a systemd unit that starts early and never stops restarting, a static `/etc/resolv.conf`, and stamps that stop every boot from rerunning `ldconfig`.
- It masks the SSH server, the resolver service and background apt updates.
- It never writes through a symlink in the unpacked image, which could point at the host's own files.

**API v1**, served over `/run/tine/tine.sock`

| Method and path | What it does |
|---|---|
| `POST /v1/vms` | Create a VM: `{image, vcpus, memory_mib, ttl_seconds}` |
| `GET /v1/vms`, `GET /v1/vms/{id}` | List VMs or get one |
| `POST /v1/vms/{id}/exec` | Run a command and stream its output |
| `DELETE /v1/vms/{id}` | Destroy a VM (idempotent) |
| `GET /v1/images` | List images |
| `GET /healthz`, `GET /metrics` | Health and Prometheus metrics |

Errors come back as `{"error": {"code": "...", "message": "..."}}` with proper HTTP status codes.

**Safety limits**

- A maximum number of VMs.
- Default size: 2 vCPUs and 1 GiB.
- A default time-to-live of 1 hour. A reaper destroys expired VMs, so forgotten sandboxes clean themselves up.

### Tasks

- [x] **1.1 Project skeleton:** Go module, the layout above, `Makefile` (`build`, `check`, `deploy`; `e2e` arrives with task 1.14), `golangci-lint` config, and GitHub Actions CI.
- [x] **1.2 Data volume:** add a 50 GB gp3 EBS volume to the dev server (about $4.60/month), formatted XFS and mounted at `/srv/tine`. Teach `dev-server.sh` and `host-setup.sh` about it. *Measured: copying a 2 GB disk image takes 2,725 ms normally and 5 ms as a reflink, using 0 MB of extra disk.*
- [x] **1.3 Firecracker client** (`internal/firecracker`): typed calls for configure, start, pause and resume over a Unix socket, with every call timed. Unit tests against a fake socket server. *Also tested against real Firecracker 1.17.0 with `make test-integration`: configuration calls take 0.1–2 ms and pause or resume about 0.25 ms (nested virtualization).*
- [x] **1.4 Jails** (`internal/jailer`): build the jailer command, prepare the chroot (hard links and reflinks), allocate a uid per VM, and clean up. *Measured on the dev server: preparing a jail (reflinking a 2 GB disk and hard-linking the kernel) takes under 1 ms. From starting the jailer, Firecracker's API answers in about 7 ms, the VM is started at about 22 ms, and the guest kernel prints its first line at about 82 ms. The guest didn't shut down within 5 s of Ctrl+Alt+Del, so tined should stop VMs by killing them.*
- [x] **1.5 Networking** (`internal/netns`): namespaces, tap, veth, routes, NAT and firewall rules in `inet tine`, and a slot allocator. E2e test: create 20, delete 20, nothing left behind. *Slots are allocated with `internal/idalloc`, sized by `netns.MaxSlots`. Measured on the dev server: creating a VM's network takes about 5 ms (median; slowest 17 ms) and destroying it about 16 ms. A real VM reached the internet, and was blocked from the EC2 metadata service, the host (through its gateway and on its main address) and another VM's network, each of which the host could reach.*
- [x] **1.6 Guest agent** (`cmd/tine-agent`): vsock HTTP server with `/health` and streaming `/exec`. Unit tests over a local socket. *The server is in `internal/agent`, and the protocol and host client in `internal/agentproto`. Tests run the real agent and client together over Unix sockets on macOS and on Linux in CI, plus the vsock handshake against a fake Firecracker socket. Real vsock inside a guest is checked in task 1.7, once the image includes the agent.*
- [x] **1.7 Images:** `tined image import base` produces the base image with the agent baked in. *Measured on the dev server: an import takes about 2 s for a 4 GiB disk. A VM booted from it answers over real vsock 2.2 s after starting, and running `true` takes 2–3 ms end to end. Of the boot, the guest kernel takes about 1.1 s and userspace 0.9 s, and the agent starts 1.7 s in. Leads for task 1.9 and step 4: Firecracker's usual fast-boot kernel arguments, and mounting the root disk read-write from the start. Templates (step 2) skip booting entirely.*
- [x] **1.8 State** (`internal/state`): SQLite schema for images and VMs, with migrations. *Tables for images, VMs and every VM state change (`vm_events`). Migrations are numbered SQL files embedded in the binary, each in a transaction, and tined refuses a database from a newer version. The VM state machine is enforced by the store, and writes take the lock up front, so of two racing "destroy" calls exactly one wins. Slots and uids are unique among VMs that aren't destroyed. Times are stored fixed-width, so comparing them as text, as SQLite does, compares them in time order.*
- [x] **1.9 VM lifecycle** (`internal/vm`): create and destroy as a state machine (`creating → running → destroying → destroyed`, plus `failed`), with rollback when a step fails. *Unit tests fail each step in turn against a fake host and check the undo. On the dev server, a create takes about 1.9 s, nearly all of it the guest booting: allocate 0.3 ms, network 10 ms, jail 1 ms, start 1 ms, configure 60 ms, agent ready 1.8 s. Booting with `rw` and the i8042 flags cut boot-to-ready from 2.2 s to 1.8 s. A destroy takes about 30 ms, 4 VMs created in parallel are all up in 2.5 s, and a forced failure rolls back leaving nothing on the host.*
- [x] **1.10 The service** (`cmd/tined`): API server, config flags, systemd unit in `deploy/`, and `/healthz`. *The API and its Go client are in `internal/api`. tined runs under systemd (`Type=notify`) and serves `/run/tine/tine.sock` to root and the `tine` group. Measured through the API on the dev server: create about 1.8 s, a tined restart 41 ms with its VMs still running, destroy about 30 ms. The restart test found a bug: destroying a VM after a restart left its Firecracker running, because the PID check expected `--id=<id>` and the jailer passes `--id <id>`. tined now stops a VM it didn't start by killing everything in that VM's own cgroup, and cleanup stops at the first failure instead of removing files under a live VM. `make test-integration` stops tined first, because the tests set up the host's VM networking themselves.*
- [ ] **1.11 The CLI** (`cmd/tine`): `create`, `ls`, `exec` (streams output and passes through the exit code), `destroy`, and a `--json` flag. It prints timings.
- [ ] **1.12 Recovery:** on startup, reconcile SQLite with the real processes, namespaces and jails. Add the TTL reaper.
- [ ] **1.13 Metrics and logs:** Prometheus histograms per stage, and structured logs with the VM ID on every line.
- [ ] **1.14 E2e suite:** create, exec and destroy 50 times; destroy during create; kill `tined` mid-operation and restart it; then the leak check.
- [ ] **1.15 Docs and release:** `docs/architecture.md`, README update, tag `v0.1.0`.

### Done when

- [ ] `tine create`, `tine exec <id> -- uname -r` and `tine destroy` work, and `exec` passes through exit codes and streams output live.
- [ ] 10 VMs run at once, each with internet access. They can't reach each other or `169.254.169.254`.
- [ ] Restarting `tined`, even while VMs are running, loses nothing and leaks nothing.
- [ ] The leak check is clean after the e2e suite.

### Measure

- Cold-boot `create` latency, p50 and p99 over 100 runs, with the stage breakdown.
- `exec` round-trip time for `true`.
- Host memory per idle VM (RSS).

---

## Step 2: Snapshots, templates and rollback

**In plain words:** add save points. A snapshot pauses a VM, saves its memory, device state and disk, then lets it carry on. Rollback throws away the VM's current state and restores a save point in its place. Templates use the same trick for speed: boot each image once, save it when it's ready, and start every new VM from that save point instead of booting from scratch.

**What you'll have at the end:**

```
$ tine snapshot vm-7f3k2q9d1a
snap-q2w9x7m1  (paused 41ms: pause 0.2ms · memory 35ms · disk 3ms · resume 0.3ms)

$ tine exec vm-7f3k2q9d1a -- rm -rf /important     # oops
$ tine rollback vm-7f3k2q9d1a --to snap-q2w9x7m1
restored vm-7f3k2q9d1a from snap-q2w9x7m1 (18ms)

$ tine create                                      # now restored from the base template
vm-a8d0c4k2pz  running  (from template in 24ms)
```

The numbers are again made up and get measured in this step.

### Design

**The snapshot store**

```
/srv/tine/snapshots/<snap-id>/
├── vmstate        Firecracker's device and CPU state
├── memory         guest memory (immutable from now on)
├── rootfs.ext4    reflink copy of the disk at that moment
└── snapshot.json  parent VM, parent snapshot, sizes, Firecracker version,
                   CPU model, kernel, created_at, timings
```

Snapshots are reference-counted. One can't be deleted while a VM is running from it, and unused ones are garbage-collected.

**Taking a snapshot**

1. `PATCH /vm {"state": "Paused"}`.
2. `PUT /snapshot/create`, with `snapshot_type: Full`, paths inside the jail, and `sync_snapshot_files: false`. Durability across a host crash isn't needed, and skipping it makes snapshots much faster. Firecracker drains and `fsync`s the disk on its own.
3. Reflink the VM's disk into the snapshot directory.
4. `PATCH /vm {"state": "Resumed"}`.
5. Move the files into the store, which is an instant rename on the same filesystem, and save the metadata.

The time between steps 1 and 4 is the VM's **pause time**, which we report separately because users feel it.

**Restoring from a snapshot**

1. Build a jail. Hard-link `vmstate` and `memory` into it, and reflink the disk.
2. Create the network namespace with the same `tap0`.
3. Start a jailed Firecracker.
4. `PUT /snapshot/load` with:
   - `mem_backend: {backend_type: File, backend_path: /memory}`
   - `resume_vm: true`
   - `track_dirty_pages` set again, because it isn't saved in the snapshot
5. Re-sync the guest's identity through the agent's new `POST /sync`:
   - Set the clock: a restored guest's clock is stuck at snapshot time. We also try Firecracker's `clock_realtime` option and keep whichever works better.
   - Mix fresh host randomness into the guest's entropy pool.
   - Set the hostname.

**Templates**

- `tine image warm <image>` cold-boots the image, waits until the agent is ready, and snapshots it as that image's template.
- `tine create` restores from the template by default.
- `tine create --cold` keeps cold boot for benchmarks.

**Rollback**

`tine rollback <vm> --to <snap>` stops the VM's Firecracker process and restores the snapshot into the **same** VM ID and network slot.

**Things to get right**

- **Taking a snapshot resets vsock connections in the running VM too.** The guest's listening sockets survive, so `tined` simply reconnects. An `exec` that's still running would lose its connection, so `snapshot` returns `409 Conflict` while an exec is in flight.
- **A memory file must never change while a VM runs from it.** Changes always go into a new file (see step 4).
- **Snapshots only restore with the same Firecracker version on the same CPU model.** Both are stored in `snapshot.json`, mismatches are refused, and templates are re-warmed after upgrades.
- **Check that the guest kernel actually reseeds its random number generator** when Firecracker's VMGenID changes, by looking at the guest's kernel log. Agent-side mixing covers us either way.

### Tasks

- [ ] **2.1 Firecracker client:** `snapshot/create` and `snapshot/load`, including all their options.
- [ ] **2.2 Snapshot store:** layout, metadata, reference counts, compatibility checks, garbage collection.
- [ ] **2.3 Snapshot command:** `POST /v1/vms/{id}/snapshots`, `tine snapshot <vm>`, and the 409 while an exec is running.
- [ ] **2.4 Restore path** in `internal/vm`, plus the agent's `/sync` (clock, entropy, hostname).
- [ ] **2.5 Templates:** `tine image warm`. `create` restores from the template, and `--cold` keeps cold boot.
- [ ] **2.6 Rollback:** `POST /v1/vms/{id}/rollback` and `tine rollback`.
- [ ] **2.7 Snapshot management:** `tine snapshot ls`, `tine snapshot rm`, `GET` and `DELETE /v1/snapshots/{id}`.
- [ ] **2.8 E2e tests:**
  - Write a file, snapshot, change it, roll back: the file is back.
  - After a restore, the guest's clock is correct.
  - Two restores of one snapshot read different random numbers.
  - Leak check.
- [ ] **2.9 Docs and release:** update `docs/architecture.md`, tag `v0.2.0`.

### Done when

- [ ] Snapshot → break something → rollback works, and the VM keeps its ID and network.
- [ ] `tine create` from a template is much faster than a cold boot, with both numbers recorded.
- [ ] A restored guest has the right time and fresh randomness.
- [ ] Snapshots in use can't be deleted, and unused ones get cleaned up.

### Measure

- Snapshot pause time and total time, for 256 MiB, 1 GiB and 4 GiB VMs.
- Restore time to "agent ready".
- `create` from a template vs. cold boot, p50 and p99.

---

## Step 3: Fork

**In plain words:** this is the headline feature. Forking means taking one snapshot of a running VM and restoring it many times at once. The original keeps running. Each copy gets its own disk and its own network. All copies share the same memory pages until one of them changes a page, the hotel trick from step 0.

**What you'll have at the end:**

```
$ tine fork vm-7f3k2q9d1a -n 5
vm-b1… vm-b2… vm-b3… vm-b4… vm-b5…   (5 forks in 140ms; parent paused 38ms)

$ tine ls --tree
vm-7f3k2q9d1a  running
└── snap-h4k8…
    ├── vm-b1…  running
    ├── vm-b2…  running
    ├── vm-b3…  running
    ├── vm-b4…  running
    └── vm-b5…  running
```

### Design

**API:** `POST /v1/vms/{id}/fork {"count": 5, "ttl_seconds": 600}` returns one result per child. Each fork succeeds or fails on its own, and failed children are cleaned up.

**Sequence**

1. Snapshot the parent, using step 2's path. The parent is only paused during this part.
2. Restore the children in parallel, with a worker pool sized to the CPU count. Each child gets:
   - its own jail, with hard links to the shared `memory` and `vmstate` and a reflinked disk
   - its own namespace, with the same addresses inside
   - a new identity through `/sync`
3. Record the lineage: which parent, which snapshot, and which children.

**Why the memory is shared:** every child's `memory` is a hard link to the same file, and Firecracker maps it privately. The host's page cache holds **one** copy of each page. A child only gets its own copy of a page when it writes to it. To show this, we report **PSS** (proportional set size, from `/proc/<pid>/smaps_rollup`) next to RSS. PSS splits shared pages fairly between the processes that share them.

**Admission control:** before forking, `tined` estimates the memory the children will need and compares it with the host's available memory and a configurable overcommit ratio. If there isn't enough, it refuses with a clear error, rather than letting the host run out of memory.

**Making each fork unique**

| Thing that's cloned | What we do |
|---|---|
| Random number state | VMGenID reseed, virtio-rng, and host randomness mixed in through `/sync` |
| Hostname | Set to the VM ID |
| `/etc/machine-id` | Regenerated by the agent |
| Clock | Set through `/sync` |
| MAC and IP address | Left identical. They're isolated per namespace. |
| Open TCP connections | Dead in children, because the other end belongs to the parent. Documented as expected behavior. |
| Running processes | Keep running in every fork. That's the point. |

### Tasks

- [ ] **3.1 Fork API:** fork with a worker pool and per-child results, cleaning up failed children.
- [ ] **3.2 Lineage:** track parents and children in state, and add `tine ls --tree`.
- [ ] **3.3 Admission control:** memory estimate, overcommit ratio, clear errors.
- [ ] **3.4 Fork identity:** machine-id, hostname and entropy through `/sync`.
- [ ] **3.5 CLI:** `tine fork <vm> -n <count>`, with timings including the parent's pause time.
- [ ] **3.6 E2e tests:**
  - Fork 10, and each child writes a different file: no child sees another's file.
  - Children can't reach each other and get different random numbers.
  - The parent keeps running and is unaffected.
- [ ] **3.7 Density test:** keep forking until admission control refuses. Nothing crashes, and the leak check is clean afterwards.
- [ ] **3.8 Docs and release:** update `docs/architecture.md`, tag `v0.3.0`.

### Done when

- [ ] `tine fork <vm> -n 10` returns 10 running, isolated children, and the parent keeps running.
- [ ] Children share memory, and PSS shows it.
- [ ] Forking past the host's limits fails cleanly instead of taking the server down.

### Measure

- Fork latency for n = 1, 10, 50 and 100.
- The parent's pause time.
- RSS vs. PSS per child.
- Maximum number of idle children per host.

---

## Step 4: Measure, then make it fast

**In plain words:** build a proper benchmark setup, record honest numbers, then make tine faster one improvement at a time, measuring each improvement the same way. This step produces the main content of the writeup: where the milliseconds go, and what breaks.

### Design

**The benchmark harness**

- `tine-bench` (Go) drives `tined`'s API through scenarios. It writes one JSON line per run, with every stage's timing and memory stats, plus host details: instance type, CPU, kernel, Firecracker version and tine commit.
- `bench/analyze.py` turns the results into charts (latency CDFs, stacked stage bars, density curves) and Markdown tables. Results go in `bench/results/<date>-<host>/`.

**Two small programs baked into the image as test workloads**

- `tine-touch` touches N MB of memory and reports the time per page. This measures how much slower a fork runs right after forking, which vendors don't publish.
- `tine-dirty` changes N MB of memory, so we can control how much memory a snapshot has to save.

**How we measure**

- Warm-up runs first, then at least 100 runs per data point. Report p50, p95, p99, minimum and maximum.
- Change one thing at a time.
- Drop the page cache before measuring "cold" cases.
- Record every version.
- Label dev-server numbers as **nested virtualization**.

**The improvement ladder.** Each rung is a strategy `tined` can switch between, so all of them can be benchmarked side by side.

| Rung | Approach | What we expect |
|---|---|---|
| 1 | Cold boot | The baseline |
| 2 | Naive fork: full snapshot, then a full byte copy of the memory for each child | Slow, and grows with memory size and number of children |
| 3 | Shared file-backed restore (step 3's default): one full snapshot, and children share it through the page cache | Children nearly free; the snapshot itself still writes all memory |
| 4 | **Diff snapshots plus reflink merge** | Fork cost depends on the memory changed, not total memory |
| 5 | **Our own page loader (`tine-uffd`)** with working-set prefetch | Fastest starts and less slowdown after a fork |

- **How rung 4 works:**
  - The parent runs with dirty-page tracking on, so a `Diff` snapshot writes only the pages it changed since its last snapshot.
  - To make that snapshot loadable, we reflink-copy the previous memory file (instant) and write the changed pages into the copy, using `snapshot-editor edit-memory rebase` or our own Go version.
  - The previous file is never touched.
  - Firecracker still calls diff snapshots a developer preview, so this stays behind a flag, with rung 3 as the fallback.
- **How rung 5 works:**
  - The restore uses Firecracker's `Uffd` memory backend. Firecracker hands our `tine-uffd` process the guest's memory map and a userfaultfd, and we load each page on demand with `UFFDIO_COPY`.
  - On a template's first restore, we record which pages the guest touches early on. Later restores load those pages in advance, an idea from the REAP research paper.
  - This also unlocks 2 MiB huge pages, which only work with `Uffd`.
- **Stretch goal:** live fork using userfaultfd write-protection, where the parent keeps running while its memory is captured. That's research-grade, and only worth doing if everything else is finished.

**Host tuning experiments:**
- transparent huge pages on or off
- KSM (merging identical pages) on or off, for density
- the balloon device with free-page reporting, so idle VMs give memory back (Firecracker v1.17 or later only, because of CVE-2026-122192)

**What breaks:** push each limit until something fails, and explain the failure. The candidates:
- running out of host memory
- the shared memory file getting pushed out of the page cache under pressure, so children suddenly read memory from disk
- limits on network devices and firewall rules
- file descriptors, process IDs and KVM limits

Every cliff gets a chart and an explanation.

**Bare-metal run:** rent a `.metal` instance for a few hours (state the hourly price before launching, and terminate it as soon as the run is done). Re-warm the templates there, run the full suite, and compare the results with the nested numbers.

### Tasks

- [ ] **4.1 Harness:** `tine-bench` scenarios, JSONL output, and host details.
- [ ] **4.2 Analysis:** `bench/analyze.py` with charts and tables.
- [ ] **4.3 Workloads:** `tine-touch` and `tine-dirty`, baked into a benchmark image.
- [ ] **4.4 Rungs 1–3:** benchmark all three. Rung 2 is a strategy flag.
- [ ] **4.5 Rung 4:** diff snapshots plus reflink merge, behind a flag.
- [ ] **4.6 Rung 5:** `tine-uffd` page loader with on-demand loading.
- [ ] **4.7 Working-set prefetch:** record on first restore, prefetch afterwards. Try huge pages.
- [ ] **4.8 Tuning experiments:** transparent huge pages, KSM, balloon.
- [ ] **4.9 "What breaks" runs:** push each limit and document every cliff.
- [ ] **4.10 Bare-metal run:** full suite, compared with nested.
- [ ] **4.11 Results and release:** `docs/results.md` with every chart and the method, reproducible with one command. Tag `v0.4.0`.

### Done when

- [ ] Every rung is measured the same way, on nested and on bare metal.
- [ ] `docs/results.md` explains where the time goes at each rung, and what breaks and why.
- [ ] Anyone with a KVM machine can rerun it with one command.

---

## Step 5: Python SDK, the AI agent demo, and the writeup

**In plain words:** make tine easy to use from Python, build the demo that shows why fast forking matters, and turn everything into a project people want to read about. The AI is a small part, a couple of hundred lines. tine is the star.

### Design

**Images from Dockerfiles**

- `tine image build python -f images/python/Dockerfile`: `docker build`, export the filesystem, pack it into ext4, add `tine-agent`, and warm a template.
- The demo needs a Python image with git and pytest.

**Agent additions:** file read and write (`GET` and `PUT /files`), and uploading a directory as a tarball.

**Using tine from your Mac:** `tine --host ssh://tine-dev …` runs `ssh tine-dev tine dial-stdio`, which connects stdin and stdout to `tined`'s socket. It's the same trick as Docker's `ssh://` hosts, so there are no open ports and no extra auth.

**The Python SDK** (`sdk/python`, package `tine`)

```python
from tine import Sandbox

with Sandbox.create(image="python") as sb:            # destroyed automatically at the end
    sb.exec("git clone https://github.com/... repo && pip install -e repo")
    forks = sb.fork(5)                                 # 5 running copies, already set up
    results = [f.exec("cd repo && pytest -q") for f in forks]
```

- There are sync and async clients, the async one for running forks in parallel.
- Results are typed, and errors are clear.
- It's tested against a fake server and against the real one.

**The demo: best-of-N bug fixing** (`examples/best-of-n`)

- **Tasks:** about 10 small repos we write ourselves, each with a bug, a failing test, and hidden tests the agent never sees. That keeps the demo reproducible and fair.
- **The agent:** a tool-use loop with the Anthropic Python SDK. Its tools are `run(cmd)`, `read_file`, `write_file` and `done()`. The model is configurable, defaulting to the latest Sonnet.
- **The flow:**
  1. Create a sandbox from the Python image and set up the repo once.
  2. Fork N copies.
  3. Run N independent agent attempts at the same time.
  4. Run the hidden tests in each copy.
  5. Keep the best passing fix and destroy the rest.
  6. Print the fix, the timings and the token cost.
- **What it measures:**
  - success rate with 1 attempt vs. 5
  - total time with fork vs. redoing the setup for every attempt
- **Stretch goal:** branch at every step (tree search) instead of only at the start.

**Optional: a live fork-tree view.** `tine top` is a terminal UI showing VMs appearing as branches and turning green or red. It's just for the demo video.

**Polish**

- README: one-paragraph pitch, architecture diagram, headline results, quick start, and a terminal GIF recorded with `vhs`.
- `docs/` covers architecture, API, results and operations.
- `LICENSE` (Apache-2.0).

**The writeup:** a blog post.
1. The problem: agents and RL need many copies of a running computer.
2. How forking works, explained simply.
3. The ladder, with charts.
4. What broke.
5. What we'd do next.

### Tasks

- [ ] **5.1 Image builder:** `tine image build` from a Dockerfile, plus `images/python`.
- [ ] **5.2 Files:** agent and API endpoints for reading, writing and uploading files.
- [ ] **5.3 Remote access:** `tine dial-stdio` and `--host ssh://…`.
- [ ] **5.4 Python SDK:** sync and async clients, the `Sandbox` class, tests, and a README with examples.
- [ ] **5.5 Demo tasks:** 10 small repos with bugs, failing tests and hidden tests.
- [ ] **5.6 Demo agent:** tool-use loop, fork N, parallel attempts, pick the winner, report.
- [ ] **5.7 Demo results:** best-of-1 vs. best-of-5, fork vs. re-setup, written into `docs/results.md`.
- [ ] **5.8 Fork-tree view** (optional): `tine top`.
- [ ] **5.9 Polish:** README, GIF, docs, license.
- [ ] **5.10 Writeup:** the blog post. Tag `v1.0.0`.

### Done when

- [ ] `python examples/best-of-n/run.py` solves the tasks with forks and prints a clear comparison.
- [ ] Someone new can read the README and understand what tine is, why it's fast, and how to run it.
- [ ] The writeup is published.

---

## Risks and unknowns

| Risk | What we do about it |
|---|---|
| Nested virtualization behaves or performs differently from bare metal (userfaultfd, dirty-page tracking, timing) | Label nested numbers. Do the final runs on bare metal. |
| Diff snapshots are a developer preview in Firecracker | Keep them behind a flag, with full snapshots (rung 3) as the safe default |
| The guest kernel might not reseed randomness on VMGenID | Check early in step 2. The agent's entropy mixing covers it either way. |
| vsock connections reset on every snapshot | `tined` reconnects with retries, and snapshots refuse while an exec is running |
| Hard links and reflinks need everything on one filesystem | Everything lives under `/srv/tine` on the XFS volume |
| Snapshots only restore on the same Firecracker version and CPU | Check metadata, and re-warm templates after upgrades or on new hosts |
| Under memory pressure, the shared memory file can be evicted from the page cache | Measure it in step 4 and explain it. Huge pages or pinning are possible fixes. |
| Guests are untrusted code | Jailer, one namespace per VM, a blocked metadata service, no VM-to-VM traffic, and Firecracker kept up to date |
| Cloud costs creep up | Stop the dev server when pausing work, check `status` at the end of every session, and spend nothing outside this plan |

## Glossary

| Term | Meaning |
|---|---|
| **Host / guest** | The host is the server. A guest is a VM running on it. |
| **Daemon / service** | A program that keeps running in the background. `tined` is ours. |
| **vsock** | A direct socket between the host and a VM that doesn't need networking |
| **Network namespace** | A separate copy of Linux's networking, with its own devices, addresses and rules |
| **tap / veth** | Virtual network cables. A tap connects the host to a VM. A veth pair connects two namespaces. |
| **NAT** | Rewriting addresses so many machines can share one internet connection, the way a home router does |
| **Jailer** | Firecracker's helper that locks each VM into its own folder (chroot), user and resource limits |
| **Reflink** | A copy that shares data with the original until one of them changes. Instant, even for big files. |
| **Snapshot** | A VM's saved memory, device state and disk at one moment |
| **Template** | A snapshot of a freshly booted image, used to start new VMs quickly |
| **Fork** | One snapshot restored many times, giving many running copies of one VM |
| **Page / page cache** | Memory is handled in 4 KiB pages. The page cache is the host's in-memory copy of file contents, shared by everyone reading the same file. |
| **Copy-on-write** | Share until someone writes, then copy only what changed |
| **RSS / PSS** | Memory a process uses. RSS counts shared pages in full. PSS splits them between the processes sharing them. |
| **Dirty pages** | Pages that changed since a given moment, such as the last snapshot |
| **userfaultfd (UFFD)** | A Linux feature that lets our own program decide how a missing memory page gets loaded |
| **p50 / p99** | The median, and the value 99% of runs are faster than. p99 shows the slow cases users notice. |
