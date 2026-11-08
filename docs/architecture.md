# tine architecture

This is how tine works as of **v0.4.0**, the end of step 4: one service on a Linux server that creates microVMs, runs commands in them, snapshots them, rolls them back, starts new ones from pre-booted templates in about a twentieth of a second, **forks running VMs into many copies that share their memory**, and destroys them, safely and without leaking anything. Step 4 added the other ways to do it, to measure them side by side: diff snapshots, and page servers that load a VM's memory on demand or in advance. How they compare, and what breaks, is in [results.md](results.md).

Each section starts in plain words and then gives the detail. The *why* of the project is in [BRIEF.md](../BRIEF.md), and the step-by-step plan, including what comes after v0.4.0, is in [PLAN.md](PLAN.md).

## Contents

- [The big picture](#the-big-picture)
- [The code](#the-code)
- [Life of a VM](#life-of-a-vm)
- [Snapshots, templates and rollback](#snapshots-templates-and-rollback)
- [Forks](#forks)
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

**In plain words:** a program called `tined` runs on the server and owns every VM. You ask it for things with the `tine` command. For each VM, tined builds a locked box (its own files, its own user, its own slice of CPU and memory, its own network) and starts Firecracker inside it. Firecracker runs the guest: a real Linux kernel on virtual hardware. Inside every guest, a small program called `tine-agent` runs commands for tined. tined talks to it over vsock, a direct pipe between host and guest, so no SSH or guest network is involved. tined can also freeze a VM and save it as a snapshot, then start new VMs from it, or put the VM back to it.

```
 you
  │  tine create / exec / snapshot / rollback / fork / ls / destroy
  ▼
 tine (CLI) ──── HTTP over /run/tine/tine.sock ────┐
                                                   ▼
┌─────────────────────────────── tined ─────────────────────────────────────┐
│ api      HTTP routes, errors, request IDs, /metrics                       │
│ vm       create: boot or restore (with undo) · snapshot, full or diff ·   │
│          rollback · fork · admission control · destroy · exec ·           │
│          templates · page servers · reconcile · reaper and snapshot GC    │
│ state    SQLite: VMs, images, snapshots, every state change               │
└──────┬──────────────────┬─────────────────────┬───────────────────────────┘
       │ netns            │ jailer               │ firecracker + agentproto
       ▼                  ▼                      ▼
 ┌───────────── for each VM ──────────────────────────────────────────────┐
 │ network namespace tine-<id>   tap0 ─ veth ─ host, nftables rules       │
 │ jail /srv/tine/jail/firecracker/<id>/root   own uid, own cgroup        │
 │   firecracker ── KVM ── guest Linux                                    │
 │      │                    └─ tine-agent (vsock port 1024)              │
 │      └─ tine-uffd: serves its memory, with the uffd strategies         │
 └────────────────────────────────────────────────────────────────────────┘
 /srv/tine/snapshots/<snap-id>/   vmstate, memory, rootfs.ext4: read-only,
                                  hard-linked into the jails of VMs restored from it
```

Some properties that hold throughout:

- **tined is the only thing that changes VMs.** The CLI is a thin client of tined's HTTP API, the same split as `docker` and `dockerd`.
- **VMs don't depend on tined staying up.** Each Firecracker runs in its own cgroup, outside tined's, so restarting or even killing tined leaves VMs running. When tined starts, it reconciles its records with what's actually on the host.
- **A snapshot never changes.** Its files are read-only, and every VM restored from it maps the same memory file, so the host's page cache holds it once. That's how a fork's children share their parent's memory.
- **Nothing is admitted that the host can't take.** Every create and fork passes admission control on memory and vCPUs first, so a request too big for the host is refused, rather than taking the host down.
- **Every operation is timed.** Each create, snapshot and rollback reports a `timings_ms` breakdown, and the same numbers feed Prometheus.

## The code

The binaries, all Go, built on the Mac (or in CI) for linux/amd64 and run on the server:

| Binary | What it is |
|---|---|
| `cmd/tined` | The service. `tined serve` runs under systemd; `tined image import` builds images. |
| `cmd/tine` | The CLI: `create`, `ls` (and `ls --tree`), `inspect`, `exec`, `snapshot` (and `snapshot ls`, `inspect`, `rm`), `rollback`, `fork`, `destroy`, `images`, `image warm`, `version`. |
| `cmd/tine-agent` | Runs inside every guest and serves `/health`, `/exec` and `/sync` over vsock. |
| `cmd/tine-uffd` | A page server: serves one VM's memory from a snapshot, page by page, through userfaultfd (see [Fork strategies and page servers](#fork-strategies-and-page-servers)). |
| `cmd/tine-bench` | The benchmark harness: drives tined's API through every scenario of step 4 and writes a JSON line per run (see [results.md](results.md)). |
| `cmd/tine-touch`, `cmd/tine-dirty` | Benchmark workloads, baked into the `bench` image: time each page a guest touches, and change a given amount of memory. |

The packages under `internal/`:

| Package | Job | Imports from tine |
|---|---|---|
| `api` | HTTP server and Go client for tined's API | `vm`, `state`, `image`, `agentproto`, `metrics`, `logging` |
| `vm` | The VM manager: lifecycle with undo, snapshots, restores, templates, rollback, forks, admission control, reconcile, reaper and snapshot GC | everything below |
| `state` | SQLite store, migrations and the VM state machine | none |
| `jailer` | Builds a VM's jail and the jailer command line | `storage` |
| `netns` | A VM's network namespace, devices and firewall rules | none |
| `firecracker` | Typed client for Firecracker's API socket | none |
| `agentproto` | The host ↔ guest protocol, and the host's client for it | none |
| `agent` | The server inside the guest | `agentproto` |
| `image` | Builds and reads images | none |
| `snapshot` | The snapshot store: files, metadata, compatibility checks | none |
| `storage` | Reflink copies, hard links, plain copies, and merging a diff onto its base | none |
| `uffd` | The page server behind `tine-uffd`, and the working-set format | none |
| `workload` | What `tine-touch` and `tine-dirty` do to memory | none |
| `idalloc` | Allocates network slots | none |
| `metrics`, `logging` | Prometheus metrics; structured logs with request and VM IDs | none |
| `testenv` | Helpers that find test artifacts and skip tests that can't run | none |

`vm` is the one place that knows about everything. The packages below it each do one job and know nothing about VMs as a whole, so each can be tested on its own, most against fakes and all against the real thing on the server.

## Life of a VM

**In plain words:** a VM is a row in a database that moves through a few states. tined builds the VM one stage at a time, and if any stage fails it undoes the earlier ones in reverse order, so a failed create leaves nothing behind. A VM starts in one of three ways, which its record keeps as its `source`: booted (`cold`), restored from its image's `template`, or restored from another `snapshot`.

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

`POST /v1/vms` restores the VM from its image's template if there's a usable one (see [Restoring](#restoring)), and otherwise boots it. Booting runs these stages in order. The times are from 100 cold creates, one at a time, on the dev server ([measured numbers](#measured-numbers)).

| Stage | What happens | p50 |
|---|---|---|
| allocate | Check the image and the VM limit. Pick an ID (`vm-` plus 10 base32 characters), a network slot and a uid. Record the VM as `creating`. | 0.32 ms |
| network | Create the namespace `tine-<id>` with `tap0`, the veth pair, routes, NAT and firewall rules. | 5.2 ms |
| jail | Create the chroot, hard-link the kernel, and reflink-copy the 4 GiB disk. | 0.32 ms |
| start | Run the jailer, which sets up the cgroup and namespace and becomes Firecracker. | 0.20 ms |
| configure | Over Firecracker's API: logger, machine config, kernel and boot arguments, root drive, network interface, vsock, entropy device, then `InstanceStart`. | 21 ms |
| agent | The guest kernel and systemd boot until `tine-agent` answers `/health` over vsock. | 1.81 s |
| sync | The agent's `/sync` sets the clock, entropy, hostname and machine ID (see [Restoring](#restoring)). | 5.3 ms |
| **total** | | **1.84 s** (p99 1.91 s) |

Almost all of a cold create is the guest booting, which is why templates exist: a create from one takes 111 ms.

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

`tine exec` exits with the command's own code, 128+n for a signal, and 124 for a timeout. Running `true` takes **1.8 ms** round trip through the API, of which about 1 ms is inside the guest. An exec that arrives while the VM is being snapshotted or rolled back waits for that to finish.

## Snapshots, templates and rollback

**In plain words:** a snapshot is a save point. tined freezes a VM for a moment, writes its memory and devices to files, copies its disk (instantly, as a reflink), and lets it carry on. A VM can then be started from a snapshot instead of booting, which is called a *restore*. It wakes up exactly where the snapshot left it, programs and all. Templates use this for speed: each image is booted once and saved once it's ready, and new VMs start from that save point in about a tenth of a second. Rollback puts a VM back to one of its save points.

### Taking a snapshot

`POST /v1/vms/{id}/snapshots`, or `tine snapshot VM`, runs these stages. The times are for a 1 GiB VM.

| Stage | What happens | p50 |
|---|---|---|
| wait | The VM's previous snapshot must be on disk first, and for 2 s (see below) | 0 to 10 s |
| prepare | Give the jail an empty `/snapshot` directory for Firecracker to write into | under 0.1 ms |
| pause | `PATCH /vm {"state": "Paused"}`: the vCPUs stop | 0.3 ms |
| memory | `PUT /snapshot/create`: Firecracker writes `vmstate` and `memory`, without waiting for the disk (`sync_snapshot_files: false`) | about 160 ms |
| disk | Reflink the VM's disk into the snapshot | 0.3 ms |
| resume | `PATCH /vm {"state": "Resumed"}` | 0.5 ms |
| collect | Move the two files into the store (renames: one filesystem) | under 0.1 ms |
| agent | Taking a snapshot resets the guest's vsock connections, so check that its agent answers again | 1 to 5 ms |

`paused`, the time from pause to resume, is reported on its own, because it's the part a VM's user notices.

The manager runs each step itself through small host calls, so unit tests can fail each step in turn. Whatever fails after the pause, the VM is resumed, and a VM that can't be resumed is marked `failed`. Once the VM is paused, the snapshot runs to the end even if the client hangs up.

A snapshot is refused (409 `busy`) while commands run in the VM, because the vsock reset would cut them off. A command that arrives during a snapshot waits for it. A VM may have 16 snapshots, and a snapshot must leave 2 GiB of disk free (507 `no_space`).

**Snapshots and the page cache.** Firecracker writes the memory file through the page cache, and Linux charges those pages to the writer's cgroup, which is the VM's. With the step 1 limit (memory plus 256 MiB), a second snapshot made the kernel write the first one's dirty pages to disk while the VM was paused, which took 11.7 s, or kill Firecracker for running out of memory. The real-VM test found this. Now:

1. Each VM's `memory.max` has room for one snapshot's memory file: twice its memory, plus 256 MiB.
2. Once the VM is running again, tined flushes the new memory file to disk in the background, so its cached pages become clean and the kernel can drop them at no cost.
3. The VM's next snapshot waits for that flush before pausing it (the `wait` stage).

On this EBS volume a flush takes about 8 s per GiB, so back-to-back snapshots of one VM go at the disk's speed. That wait comes before the pause, not during it.

4. Then it waits until that flush is 2 s old. Started just after a big flush, a snapshot's writes were throttled to the disk's speed, pausing a 256 MiB VM for 340 ms to 1.3 s instead of 50 ms, in one or two of every three back-to-back snapshots. It wasn't the cgroup's room or the page cache: two seconds after the flush, every snapshot paused for about 50 ms, whether the previous snapshot's pages were still cached or not. With the wait, nine back-to-back snapshots all paused it for 48 to 51 ms. Step 3's measurements missed this, because they deleted each snapshot straight away.

**The kernel's dirty-page limit.** Until those pages are on disk they're dirty, and the kernel throttles anything that writes more when a cgroup has too many. At the default `vm.dirty_ratio` of 20%, that held a 4 GiB VM's snapshot to the disk's write speed, pausing the VM for 22 s. It also throttled a fork's parent: Firecracker handles disk and vsock on one thread, so the parent's agent stopped answering for seconds while its fork's snapshot was written out. `scripts/host-setup.sh` sets 60% in `/etc/sysctl.d/90-tine.conf`, and tined warns at startup when the setting is lower. The 4 GiB pause is now about 0.6 s (p50), and the parent no longer stalls. Background writeback still starts at `dirty_background_ratio` (10%).

### Diff snapshots

**In plain words:** a full snapshot writes all of a VM's memory, so the pause grows with the VM. A diff snapshot writes only the memory that changed since the VM's last save point, then builds the full memory file afterwards, once the VM is running again.

With `--diff-snapshots`, every Firecracker tined starts tracks the pages its guest writes (KVM's dirty log). Each VM has a *base*: the snapshot its memory has changed from. That's the snapshot it was restored from (a template, say), and then each snapshot taken of it.

1. **Before the pause**, tined hard-links the base's memory file into the new snapshot's directory, so the base can't be deleted from under it.
2. **Paused**, Firecracker writes a `Diff`: a sparse file the size of the memory, with only the changed pages written and holes for the rest. The dirty log starts again.
3. **Running again**, tined makes the new snapshot's memory: the base's, with the diff's data written over it at the same offsets (`storage.ApplyDiff`, which finds the data with `SEEK_DATA` and `SEEK_HOLE`). For a snapshot kept for later, the base is a reflink, which shares the unchanged pages with it on disk. For a fork, whose children restore from it at once, the base is copied through the page cache instead: a reflink's pages aren't in the cache, and the children would read them from the disk. `--diff-merge` picks one for both.

The result is a whole memory file, like any other, so restoring, forking and deleting work as before, and the two snapshots are independent. A VM gets a full snapshot when it has no base: it was booted, it ran before tined restarted (tined can't know since when its pages are tracked), or its last snapshot failed after Firecracker began writing, which may have reset the tracking. Firecracker calls diff snapshots a developer preview, hence the flag. tined checks at startup that the snapshot store's filesystem reports holes: one that doesn't would make the holes read as zeros, and the merge would write them over the base.

### The snapshot store

```
/srv/tine/snapshots/<snap-id>/        root, 0700
├── vmstate        Firecracker's device and CPU state     0444
├── memory         guest memory: never changes again        0444
├── rootfs.ext4    reflink copy of the disk at that moment  0400
├── snapshot.json  metadata                                 0444
└── workingset     pages a restore touched, if recorded     0400
```

- **Atomic.** A snapshot is gathered in `.new-<id>` and renamed into place when it's complete, so a directory named after a snapshot always holds a whole one.
- **Read-only, owned by root.** Restored VMs get hard links to `vmstate` and `memory`, which they can read but not write. Firecracker requires the memory file to stay unchanged while any VM runs from it.
- **Compatibility.** `snapshot.json` records the Firecracker version, CPU model and host kernel. Firecracker only restores a snapshot on the same software and hardware it was taken on, so all three must match (409 `incompatible`). It also records which build of the image the VM ran, with the size, the bytes of each file and the stage timings.
- **Records.** The `snapshots` table holds each snapshot's kind (`vm` or `template`) and state (`creating`, `ready`, `deleting`). Each VM's `snapshot_id` names the snapshot it runs from. "In use" is a query, the live VMs with that `snapshot_id`, rather than a counter that could drift. Claiming a snapshot for a restore and marking it for deletion are each one transaction, so they can't race; a test runs them against each other 20 times.

### Restoring

A create from a snapshot runs the same first stages as a boot, then loads instead of booting:

| Stage | What happens | p50 |
|---|---|---|
| allocate | As for a boot, and the record claims the snapshot | 0.5 ms |
| network | As for a boot | 4.4 ms |
| jail | Hard links to `vmstate` and `memory` under `/restore/`, and a reflink of the snapshot's disk at `/rootfs.ext4` | 0.3 ms |
| start | As for a boot | 0.2 ms |
| restore | Over Firecracker's API: the logger, then `PUT /snapshot/load` from the memory file, resuming the VM | 13 ms |
| agent | Until the guest's agent answers | 61 ms |
| sync | `/sync` (below) | 32 ms |
| **total** | from the template, one at a time (n=100) | **111 ms** (p99 145 ms) |

- **The same paths as the original.** A restore needs every device at the path the snapshotted VM had it. Every jail has `/rootfs.ext4` and `/vsock.sock`, and every namespace has `tap0`, so nothing in the guest needs changing.
- **Out of the way of new snapshots.** The restored files sit under `/restore`, apart from `/snapshot` where Firecracker writes new ones, so a snapshot can never be written over the files a VM runs from. Being read-only, they couldn't be anyway.
- **Most of the time is first touches of memory.** Restored memory is mapped from the file and loaded page by page as the guest touches it. Each first touch is a page fault, which nested virtualization makes expensive. Bare metal and prefaulting are for step 4.

**`/sync` makes a restored guest current and unique.** tined calls it after every create, booted ones included, so every VM is the same in these ways however it started:

| A restored guest has | What fixes it |
|---|---|
| the snapshot's wall clock, behind by however long ago the snapshot was taken | `/sync` sets the clock to the host's. The guest keeps time with the TSC, so Firecracker's `clock_realtime`, which moves kvmclock, made no difference, and it stays off. |
| the snapshot's kernel random state | Firecracker's VMGenID device: the guest kernel reseeds on restore and logs "crng reseeded due to virtual machine fork". `/sync` also credits 64 bytes of host entropy and forces a reseed (`RNDADDENTROPY`, `RNDRESEEDCRNG`). |
| the snapshot's hostname and machine ID | `/sync` sets the hostname to the VM's ID (in the kernel, `/etc/hostname` and `/etc/hosts`), and the machine ID to one derived from the VM's ID: unique to each VM, and the same after a rollback. |
| the gateway's MAC in its ARP cache | Nothing to fix: every namespace's `tap0` has the same MAC, `06:00:AC:10:00:01`. |

What `/sync` can't change is anything a program worked out before the snapshot, such as random numbers a process cached, and anything the kernel set up once at boot, such as its `boot_id` and its secrets for TCP sequence numbers. Every restore of one snapshot shares those, and open TCP connections don't survive. Firecracker's VMClock device lets programs notice a restore themselves.

### Templates

- **Warming** (`tine image warm IMAGE`, `POST /v1/images/{name}/warm`): tined boots the image, waits for `systemctl is-system-running --wait` so every service has started, waits 2 s more, snapshots it as a `template`, and destroys the VM. It takes about 8.5 s. The 2 s matter: systemd says the boot is finished as it starts its last units, so a template saved at once had every restore finish that work, and creates took 125 ms instead of 62 ms. A snapshot fixes a VM's size, so a template is for one image at one size.
- **Choosing.** `tine create` uses the newest template for its image and size, if this host can restore it and the image hasn't been imported again since. Otherwise it boots, and notes why on the VM. `--cold` always boots. Each VM records how it started: `cold`, `template` or `snapshot`.
- **Keeping them usable.** tined keeps the images in `--warm-images` (default `base`) warm by itself. At startup and every 15 s, a missing or unusable template is warmed again in the background, so templates come back after a Firecracker upgrade, a kernel upgrade or a re-import. After a failure it waits 10 minutes, and warms of one image and size never overlap.

### Rollback

`POST /v1/vms/{id}/rollback`, or `tine rollback VM --to SNAPSHOT`, puts a VM back to one of its own snapshots, or to the template it started from:

1. Claim the snapshot, so it can't be deleted.
2. Stop the VM's Firecracker (through its cgroup) and remove its jail. The network namespace stays.
3. Build a new jail from the snapshot, load it, wait for the agent, and sync.

The VM keeps its ID, network namespace, slot, uid, hostname and machine ID; everything since the snapshot is gone. A rollback takes 127 ms (p50). It's refused while commands run, a command that arrives during one waits, and a rollback that fails after the old VM is stopped marks the VM `failed`.

### Deleting snapshots, and garbage

- `DELETE /v1/snapshots/{id}`, or `tine snapshot rm`, is refused (409 `in_use`) while any live VM runs from the snapshot, and while it's still being taken.
- Every 15 s the reaper deletes what nothing needs any more, once no VM runs from it: the snapshots of destroyed VMs, and templates that a newer one replaced or whose image is gone from the disk. Whether an image is gone is checked on disk, because the images table only knows the images registered at startup; a test caught the first version deleting a newly imported image's template.
- So snapshots never outlive their VMs, whose time to live is an hour by default.

## Forks

**In plain words:** forking makes running copies of a VM. tined takes one snapshot of it, which freezes it for about a twentieth of a second, and restores that snapshot many times at once. Every copy, a *child*, wakes up exactly where the VM was, with the same files and the same programs running, but its own ID, network, disk and identity. The children share the VM's memory until one of them changes a page, so ten children cost little more than one.

### Forking

`POST /v1/vms/{id}/fork {"count": 10}`, or `tine fork VM -n 10`:

1. **Admit** all the children at once (see [Admission control](#admission-control)), before the parent is touched.
2. **Snapshot the parent**, with step 2's path, as a snapshot of the kind `fork`. Only this pauses the parent.
3. **Restore the children**, `--fork-parallelism` (the host's CPU count) at a time. Each goes through the usual create path from the fork's snapshot, so each succeeds or fails on its own, and a failed one undoes itself.
4. **Record the lineage**: each child's parent and snapshot.

The answer is 201 if any child was made, with each child's VM or error. Once the parent is snapshotted, the fork finishes even if the client hangs up.

| Children of a 256 MiB VM | Total (p50) | Parent paused | Restoring the children |
|---|---|---|---|
| 1 | 136 ms | 62 ms | 72 ms |
| 10 | 397 ms | 62 ms | 333 ms |
| 30 | 1.02 s | 64 ms | 961 ms |

The parent's pause doesn't grow with the number of children; restoring them goes at about 32 ms per child, four at a time. A fork of 10 of a 1 GiB, 2-vCPU VM takes about 870 ms, with the parent paused about 230 ms.

**A fork's snapshot** is garbage as soon as no child runs from it, even while its parent lives, so forking one VM again and again doesn't pile snapshots up. Between being written and being claimed by its first child, it would look like garbage, so the fork *pins* it: the reaper skips pinned snapshots, and deleting one is 409 `busy`. A child can roll back to its fork's snapshot, back to the moment it was made.

### Sharing memory

Every child's jail has a hard link to the same snapshot memory file, which Firecracker maps privately. The host's page cache holds each page of it once, however many children read it. A child gets its own copy of a page only when it writes to it.

RSS counts every page a process has in memory, shared or not, so it overstates what children cost. PSS (proportional set size) divides each shared page between the processes sharing it. tined reads both from `/proc/<pid>/smaps_rollup` of the Firecracker in each VM's cgroup. They're on `GET /v1/vms/{id}` and on lists with `?memory=true`, and `tine ls --tree` shows them:

```
VM                             STATE    IMAGE  MEMORY  RSS      PSS     AGE
vm-xqqe5fhgxj                  running  base   1 GiB   1 GiB    1 GiB   2s
└─ snap-5b7nypfznt (5 forked)
   ├─ vm-bspt6ta7eb            running  base   1 GiB   127 MiB  36 MiB  2s
   ├─ vm-7lkl5qqwiz            running  base   1 GiB   127 MiB  36 MiB  2s
   …
```

Each of 30 idle children of a 256 MiB VM has an RSS of 100 MiB but a PSS of 14 MiB. The parent's RSS is its whole memory just after a fork, because taking the snapshot made Firecracker read every page. For a parent restored from a template, those are mostly the template's file in the page cache, which the kernel can reclaim.

### Fork strategies and page servers

**In plain words:** sharing the parent's memory file is one way to give children their memory, and step 4 measures it against the others. Creates from a template choose the same way. `--strategy` on `tine fork` and `tine create` picks one:

| Strategy | The child's memory | Shared between children |
|---|---|---|
| `share` (the default) | Maps the snapshot's memory file privately: pages load from the page cache on first touch, and a write makes a private copy | Yes, until a child writes |
| `copy` | Its own full copy of the memory file, made before it starts: the naive way, kept to compare against | No |
| `uffd` | Anonymous memory, filled by a page server, a page at a time as the guest touches it | No |
| `uffd-prefetch` | As `uffd`, and the pages a restore of this snapshot touched before are copied in before the guest asks | No |

Rung 1 of the ladder, booting each VM, isn't a strategy: it's `tine create --cold`.

**Page servers.** With `uffd`, Firecracker restores with its `Uffd` memory backend. It maps the guest's memory as empty anonymous memory, registers it with a userfaultfd, connects to a Unix socket in the jail, and sends the memory's layout with the userfaultfd attached. The page server, `tine-uffd`, maps the snapshot's memory file read-only and shared (one copy in the page cache, whatever the number of VMs), and answers each fault by copying the page in with `UFFDIO_COPY`. Firecracker resumes the VM straight away; pages arrive as they're needed. Pages the guest gives back through the balloon (`UFFD_EVENT_REMOVE`) read as zeros afterwards.

tined starts one page server per VM, once the VM's Firecracker is up:

- **As the VM's user, in the VM's cgroup** (`clone3` with `CgroupFD`), so its memory and threads count against the VM's limits, and stopping the VM stops it.
- **With open files, not paths:** the listening socket, the memory file, a control socket to tined, and maybe a working set. The VM's user can't reach tined's directories.
- **In its own session**, so it outlives tined restarting, as the VM does. It exits when Firecracker does.

**Working sets.** A page server records which pages the guest faulted on. With `uffd-prefetch`, the first restore of a snapshot asks for them once the VM is synced, and tined saves them next to the snapshot's files as `workingset`, a bitmap of the memory file's pages. Later restores hand it to their page server, which copies those pages in, a few MiB per ioctl, while it serves faults. A fork with `uffd-prefetch` restores its first child alone, to record, then the rest; a template records on its first create. The idea is from the REAP paper.

**The trade-off.** A page server copies every page the guest touches into memory of the VM's own, where `share` maps the page cache. So children cost the host their whole touched memory, not a share of it, but a child's first write to a page is an ordinary write, not a copy-on-write fault. `--page-server-huge-pages 2M` backs that memory with 2 MiB pages from the host's hugetlbfs pool (Firecracker allows them only with `Uffd`), and `--page-cluster` copies more than one page per fault.

### Admission control

Before a create or a fork touches the host, three rules must hold for the VMs it would add:

| Rule | Default | Why |
|---|---|---|
| All live VMs' guest memory, plus the new ones', within `--overcommit` × the host's memory | 4, so 62.8 GiB on the dev server | VMs rarely use all their memory, and forks share most of theirs, so committing more than the host has is the point. The ratio bounds the worst case, of every guest filling its memory. |
| The host's available memory, less what the new VMs will use at first, above `--min-free-memory-mib` | 1 GiB, counting 192 MiB per boot and 64 MiB per restore (measured) | Keeps the host itself out of trouble now. |
| All live VMs' vCPUs, plus the new ones', within `--cpu-overcommit` × the host's CPUs | 8, so 32 on the dev server | A host can only run so many guests, even idle ones (see below). |

Requests in flight hold a reservation, so concurrent ones can't take the last room together: six creates with room for three admit exactly three. A fork is admitted as a whole, before its parent is paused. A refusal is 503 `no_memory` or `no_cpu`, saying which rule and with what numbers:

```
not enough memory: 100 more VMs of 1024 MiB would commit 103424 MiB of guest memory, more than the
limit of 62804 MiB (4.0 times the host's 15701 MiB); the VMs there now commit 1024 MiB
```

**Density.** The density test forks a 1-vCPU, 256 MiB VM until even one more child is refused, and checks nothing broke. On the dev server, the vCPU rule refuses after 30 children. The 30 children use 946 MiB of PSS between them, with 14 GiB of host memory still available. So here the CPUs limit density, not the memory, for a reason that's the dev server's: under EC2's nested virtualization, KVM starts failing to enter guests (`KVM_EXIT_FAIL_ENTRY`, VMX error 7) once about 60 1-vCPU VMs, or 20 2-vCPU ones, are running, whether booted or restored. The vCPU limit keeps the host below that. Forks of 50 and 100 children, and the real limit on memory, are for the bare-metal host of step 4.

## Isolation: what keeps a VM in

**In plain words:** tine runs code nobody has checked, so each VM sits inside several separate walls. Breaking out of one still leaves the others.

| Layer | What it does |
|---|---|
| KVM | The guest runs its own kernel on virtual hardware. A guest exploit has to escape the hypervisor, not just a container. |
| Firecracker | A minimal virtual machine monitor: a few virtio devices and a serial console, with its built-in seccomp filters (tine doesn't turn them off). |
| Jail: chroot | Firecracker sees only its own jail: the kernel, its disk, two sockets and a log. No other VM's files. |
| Jail: uid | Each VM's Firecracker runs as its own unprivileged user, `200000 + slot`. The jail's files belong to it with mode 0700. |
| Jail: cgroup | `cpu.max` (the VM's vCPUs), `memory.max` (twice the guest's memory plus 256 MiB, with room for one snapshot's memory in the page cache) and `pids.max` (128) for each Firecracker. |
| Snapshot files | Read-only and owned by root. A restored VM's jail has hard links to them that its Firecracker can read but not write. |
| Page server | Runs as the VM's user, in the VM's cgroup, with only the files tined handed it: the VM's own socket, the snapshot's memory (read-only) and its control socket. It holds the VM's userfaultfd, and nothing of any other VM. |
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

A VM restored from a snapshot has no kernel in its jail; it has `restore/vmstate` and `restore/memory` instead, hard links to the snapshot's files, owned by root and read-only.

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

- **Inside every namespace:** `tap0` is `172.16.0.1/30` with MAC `06:00:AC:10:00:01`, and the guest is `172.16.0.2` with MAC `06:00:AC:10:00:02`. The MACs are the same everywhere too, so a restored guest's ARP cache is still right. The guest's address is set on the kernel command line (`ip=172.16.0.2::172.16.0.1:255.255.255.252::eth0:off`), so it's up before userspace starts.
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
- The agent serves HTTP/1.1 on it: `GET /health`, `POST /exec` and, since protocol version 2, `POST /sync`. The request and response types are defined once in `agentproto`, which both sides import.
- tined dials a fresh connection for each request. There's nothing to reconnect after a snapshot, which resets vsock connections; the agent's listening socket survives it.
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
├── snapshots/<snap-id>/                 vmstate, memory, rootfs.ext4, snapshot.json
└── jail/firecracker/<vm-id>/root/       one jail per VM (shown above)
/sys/fs/cgroup/tine/<vm-id>/             each VM's cgroup (tine/ is root-only, 0750)
/var/run/netns/tine-<vm-id>              each VM's network namespace
/var/lib/tine/tine.db                    SQLite state
/run/tine/tine.sock                      the API socket (root:tine, 0660)
```

## State, restarts and recovery

**In plain words:** tined writes down everything it does in a small database. If it crashes or restarts, it compares those notes with what's really running and fixes any mismatch before it accepts new work. VMs keep running while it's gone.

**The store** is SQLite (`modernc.org/sqlite`, pure Go, no CGO) in WAL mode:

- **Tables:** `images`, `vms` (each VM's `snapshot_id`, `source` and `parent_id` too), `snapshots` (of three kinds: `vm`, `template` and `fork`), and `vm_events`: a history of every state change with its reason, and notes such as "snapshot taken", "guest synced: its clock was 3.4s behind" or "booted, not restored: template … was imported again since".
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
| `running`, paused | tined stopped mid-snapshot | Resume it, and clear the snapshot's files out of its jail. If its Firecracker is still writing that snapshot and doesn't answer, do this in the background once it does; commands for the VM wait. |
| `running`, Firecracker gone | It died while tined was down | Clean it up as failed |
| `failed` | Kept for inspection | Keep it until it's destroyed or expires |
| A snapshot `creating` | tined stopped mid-snapshot | Remove its files and record |
| A snapshot `deleting` | tined stopped mid-delete | Finish the delete |
| A namespace, jail, cgroup, veth or snapshot directory nothing owns | Leftover | Remove it, killing any processes in the cgroup |

It logs a one-line report, for example `kept 1; cleaned 1; orphans removed 2; errors 0`. While tined runs, it watches every Firecracker it started, and one that exits on its own marks its VM `failed`.

**The reaper** wakes every 15 s and destroys VMs past their time to live (1 hour by default), so forgotten sandboxes clean themselves up. Each time it also deletes snapshots nothing needs, and warms templates that are missing or out of date. Hourly, it purges records destroyed more than 7 days ago.

## The API

**In plain words:** tined's API is ordinary HTTP and JSON, served on a Unix socket instead of a network port. Anything that can speak HTTP can drive tine, `curl` included.

| Method and path | What it does |
|---|---|
| `POST /v1/vms` | Create a VM: `{"image", "vcpus", "memory_mib", "ttl_seconds", "cold"}`, each optional |
| `GET /v1/vms` | List VMs (`?all=true` includes destroyed ones, `?memory=true` adds each one's RSS and PSS) |
| `GET /v1/vms/{id}` | One VM, with its timings, lineage and memory |
| `POST /v1/vms/{id}/exec` | Run a command; the response streams NDJSON frames |
| `DELETE /v1/vms/{id}` | Destroy a VM (idempotent) |
| `POST /v1/vms/{id}/snapshots` | Snapshot a running VM |
| `GET /v1/vms/{id}/snapshots` | A VM's snapshots |
| `POST /v1/vms/{id}/rollback` | Roll a VM back: `{"snapshot_id"}` |
| `POST /v1/vms/{id}/fork` | Fork a VM: `{"count", "ttl_seconds"}`; each child's VM or error, the snapshot, and the timings |
| `GET /v1/snapshots` | List snapshots (`?vm=`, `?kind=`, `?image=`), with the VMs that run from each |
| `GET /v1/snapshots/{id}`, `DELETE /v1/snapshots/{id}` | One snapshot; delete one |
| `GET /v1/images` | List images, with their templates and whether each is usable |
| `POST /v1/images/{name}/warm` | Warm a template: `{"vcpus", "memory_mib"}`, each optional |
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
| 409 | `busy` | A snapshot or rollback while commands run in the VM |
| 409 | `in_use` | Deleting a snapshot that VMs run from |
| 409 | `incompatible` | A snapshot taken with another Firecracker, CPU or host kernel |
| 503 | `capacity` | The VM limit (64 by default), every network slot, or a VM's 16 snapshots are taken |
| 503 | `no_memory`, `no_cpu` | Admission control: the host hasn't room for the VMs asked for |
| 507 | `no_space` | A snapshot wouldn't leave 2 GiB of disk free |
| 500 | `internal` | Anything else; tined logs it with the request ID |

Every response carries an `X-Request-Id` header, which is also on every log line written while handling that request.

## Observability

- **Timings in every response.** A create's `timings_ms` gives each stage above, a destroy adds `destroy` and a rollback `rollback`, and a snapshot has its own stages, `paused` included.
- **Prometheus metrics** on the socket at `/metrics`, and on `127.0.0.1:9101` for a Prometheus server:
  - `tine_vm_create_stage_seconds{source,stage}`, `tine_vm_creates_total{result}`, `tine_vm_create_failures_total{stage}`
  - `tine_snapshot_stage_seconds{stage}` (with `paused` and the background `flush`), `tine_snapshots_total{kind,result}`, `tine_snapshot_failures_total{stage}`, `tine_snapshot_bytes`, `tine_snapshots_deleted_total{reason}`
  - `tine_vm_rollback_stage_seconds{stage}`
  - `tine_vm_fork_stage_seconds{stage}` (snapshot, paused, children, total), `tine_vm_fork_children_total{result}`
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

Optional behaviours, each off by default, are flags on `tined serve`, and `GET /healthz` lists the ones on as `features`. `bench/run.sh` turns them on one at a time with a systemd drop-in.

| Flag | What it does |
|---|---|
| `--diff-snapshots`, `--diff-merge` | [Diff snapshots](#diff-snapshots) |
| `--page-server`, `--page-cluster`, `--page-server-huge-pages 2M` | The page server binary, pages per fault, and huge pages for VMs it serves |
| `--balloon` | Booted VMs get a balloon with free page reporting: the guest reports free pages, and Firecracker gives them back to the host |
| `--ksm` | tined marks its memory mergeable (`PR_SET_MEMORY_MERGE`), which every Firecracker it starts inherits, so KSM can merge identical pages across VMs |

## Testing

**In plain words:** the fast tests run anywhere in seconds, using fakes. The slow tests run on the real server, against real Firecracker and real VMs, and the last of them checks that nothing at all is left behind.

| Level | Command | Runs where | What it covers |
|---|---|---|---|
| Unit | `make check` (also CI on every push) | Mac, CI | Every package, with the race detector. Fakes: a fake Firecracker socket, a fake host that fails each create, snapshot and rollback stage in turn or holds one to catch the manager in the middle, and the real agent and client over Unix sockets. |
| Integration | `make test-integration` | Dev server, as root | Each low-level package against the real thing: Firecracker (a snapshot loaded into a new process), the jailer, namespaces and nftables, reflinks and diff merges on XFS, image import, the page server against a real userfaultfd, and the VM manager booting, snapshotting, restoring, rolling back and forking real VMs: through diff snapshots, checking a restore from a merged file byte for byte, and through page servers. |
| End to end | `make e2e` | Dev server, real tined under systemd | Black-box through the API (list below), ending in a strict leak check |
| Measure | `make measure` | Dev server | Quick numbers for each feature |
| Benchmark | `make bench-all` (`bench/run.sh` on any KVM host) | Dev server, or a bare-metal host | Every scenario of step 4: see [results.md](results.md) |

The e2e suite:

- 50 creates, execs and destroys, 5 at a time
- 20 execs at once in one VM
- destroy during create
- `SIGKILL` to tined mid-create, and again with VMs running (they survive with their data)
- a VM whose Firecracker is killed while tined is down
- 10 VMs at once, checking isolation as described above
- a rollback brings a changed file back, and the VM keeps its ID, network and hostname
- a VM restored from an old template has the host's time
- two restores of one template read different random numbers, with their own hostnames and machine IDs
- a template in use can't be deleted, and a destroyed VM's snapshot is collected
- a snapshot is refused while a command runs
- `SIGKILL` to tined while it snapshots a 4 GiB VM: the VM comes back running, with nothing half-done
- a fork of 10: each child writes its own file and sees no other's, can't reach the others, reads its own random numbers and has a PSS below its RSS, and the parent carries on untouched
- a fork too big for the host is refused, with no snapshot taken
- forking until admission control refuses, then checking nothing crashed
- a VM with a 60 s time to live, which must be reaped while the rest run

At the end, the leak check requires zero live VMs, network namespaces, veths, jails, cgroups and Firecracker processes, and no snapshots but ready templates, with the store holding exactly the snapshots tined has records of. The suite takes about 3 minutes.

Bugs these tests caught before release:

- **A destroy after a tined restart left Firecracker running.** The old PID check looked for `--id=<id>`, but the jailer passes `--id <id>`, and cleanup carried on regardless. Found by the restart test. Fixed by killing through the VM's cgroup, stopping teardown at the first failure, and refusing to remove a jail whose cgroup still exists.
- **Ten racing destroys all "won".** The store accepted a change from a state to itself. Found by the store's concurrency test. Fixed by refusing same-state changes.
- **Times sorted wrongly** as text, which would have had the reaper compare expiry times out of order. Fixed by storing them fixed-width.
- **Agent tests hung in CI.** `/root` exists on Linux runners but isn't enterable by a non-root user. Fixed in the agent, which now picks a working directory it can use, and the tests got timeouts.
- **A second snapshot OOM-killed Firecracker.** Writing a snapshot's memory fills the VM's cgroup with page cache (see [Taking a snapshot](#taking-a-snapshot)). Found by the real-VM snapshot test.
- **Garbage collection deleted a new template.** It treated a template as orphaned when its image wasn't in the images table, which only knows the images there at startup. Found by a unit test; it now checks the disk.
- **A fork's parent stopped answering.** Its fork's snapshot made the kernel throttle its disk writes, and so its vsock, for seconds. Found by the fork e2e test; fixed with `vm.dirty_ratio` (see [Taking a snapshot](#taking-a-snapshot)).
- **Guests failed beyond about 60.** EC2's nested virtualization starts refusing guest entries. Found by the density test; admission control now limits vCPUs.
- **The first fork after a reboot paused its parent for 12 s.** The snapshot read the template's memory from disk. tined now reads templates into the page cache when it starts.
- **A restart couldn't resume a VM whose snapshot it had interrupted.** Firecracker was still writing 4 GiB of memory and didn't answer for 20 s. Found by the kill-mid-snapshot e2e test; reconcile now resumes it in the background.

## Measured numbers

Step 4's benchmarks, every way of giving a VM its memory side by side, the host settings and what breaks, are in [results.md](results.md), on the dev server and on bare metal. The dev server is an AWS m8i.xlarge (4 vCPUs, 16 GiB) in ap-south-1, running the VMs through **nested virtualization**, which adds overhead; the setup was Firecracker v1.17.0, guest kernel 6.18.56, and the Ubuntu 24.04 base image, with VMs at the default 2 vCPUs and 1 GiB unless noted. Its headline numbers (p50):

| What | Result |
|---|---|
| Create from the template, one at a time (n=100) | **57 ms** (p99 69 ms); 41 ms with `--strategy uffd-prefetch` |
| … setup, restore (load and resume), agent answers, sync | 5 ms, 12 ms, 20 ms, 18 ms |
| Cold-boot create, one at a time (n=100) | **1.83 s**, of which tine's own work is about 32 ms; the rest is the guest booting |
| Fork of a 256 MiB VM into 1, 10, 30 | **138 ms**, **398 ms**, **1.02 s**, with the parent paused 64 ms |
| Booting 1, 10, 30 VMs instead | 1.90 s, 7.49 s, 21.3 s |
| Each of 30 idle fork children | RSS **105 MiB**, PSS **14 MiB**, against 109 MiB for a booted VM |
| Snapshot pause: 256 MiB, 1 GiB, 4 GiB | **48 ms**, **157 ms**, **601 ms** (4 GiB p99 4.2 s); 4 to 6 ms with diff snapshots |
| A VM's next snapshot waits for the last to reach the disk | about 8 s per GiB, and 2 s more |
| Rollback (n=100) | **66 ms** |
| Exec `true` through the API (n=200) | **1.9 ms**; about 1 ms of it inside the guest |
| A fork child's first write to a page it shares | 62 µs; 1.1 µs per 4 KiB page with 2 MiB huge pages |
| Most idle children of one 256 MiB VM | **31**, set by the vCPU limit, with 14.4 GiB still available |

Measured earlier, with `make measure` and `make e2e`:

| What | Result |
|---|---|
| Destroy (n=100) | p50 **32 ms**, p99 42 ms |
| Exec `true` with the `tine` CLI (n=50) | about 6 ms, including starting the CLI |
| Host memory per idle VM, booted (n=5, after 30 s) | Firecracker RSS **127 MiB** (124 MiB private); 168 MiB charged to its cgroup, including page cache |
| Host memory per idle VM, from the template (n=5) | RSS 127 MiB, but only **19 MiB private**: 108 MiB is the template's memory file, shared; 24 MiB charged to its cgroup |
| Fork of a 1 GiB VM into 10 (e2e) | about 870 ms, the parent paused about 230 ms |
| Create with 5 at once (n=50, e2e) | p50 2.8 s, p99 3.2 s |
| tined restart with VMs running | 41 ms; the VMs keep running |
| tined back after `SIGKILL` | 2.2 s (systemd waits 2 s before restarting it) |
| Image import (4 GiB disk) | about 2.6 s |
| Reflink copy of a 2 GB disk | 5 ms (2,725 ms as a normal copy) |

A guest with 1 GiB uses about 127 MiB on the host when idle, because memory is only handed out when the guest touches it. A guest restored from a template uses less still, because most of its memory is the template's, shared through the page cache by every VM restored from it. Forks go further: an idle child of a 256 MiB VM costs 14 MiB, because nearly all its memory is its parent's, shared.

The dev server's limits are its own. Its disk is a network volume, at about 150 MB/s for writes, which the snapshots' flushes and big snapshots run into. And its VMs run through nested virtualization, which limits how many guests it can run at once and makes every first touch of a guest page slow. [results.md](results.md#nested-virtualization-against-bare-metal) compares it with bare metal.

## Known limits

These are known and deliberate for v0.4.0. Most are what the next steps are for.

- **Snapshots of one VM wait for the disk.** Back-to-back snapshots, or forks, of one VM wait about 8 s per GiB for the last one to reach the disk, and 2 s more. The VM runs meanwhile; diff snapshots shorten the pause, not this wait.
- **Diff snapshots and page servers are opt-in.** Firecracker calls diff snapshots a developer preview, so they're behind `--diff-snapshots`. A VM from before tined restarted gets a full snapshot first, as tined can't know since when its pages are tracked.
- **Page-server VMs don't share memory.** Each VM restored through a page server has its own copy of every page it touched, where `share` maps the page cache. It needs its page server for as long as it runs: if the page server dies, the guest hangs at its next untouched page.
- **A working set is recorded once.** A snapshot's working set is what its first restore through a page server touched by the time it was synced. It isn't refreshed, and a restore that then does something else gets those pages on demand.
- **`--ksm` relies on Linux 6.7 or later**, where a process's mergeable setting survives exec, and on KSM running (`/sys/kernel/mm/ksm/run`).
- **A fork's shared memory counts against its parent's cgroup.** The parent wrote the fork's memory file, so the kernel charges its pages to the parent. The parent has room for one fork's worth, so forking it again while the first fork's children run makes the kernel reclaim pages they share, which pauses the parent for longer (1.4 s seen for 256 MiB).
- **Density on the dev server is set by nested virtualization.** About 60 guest vCPUs are the most it runs before KVM fails, so tine's vCPU limit stops at 32. Memory would allow far more children. Forks of 50 and 100 children wait for bare metal.
- **A template is for one size.** A create of another size boots, unless that size is warmed too (`tine image warm --vcpus N --memory M`), and only `--warm-images` at the default size are kept warm by themselves.
- **Restores and forks share what programs and the kernel worked out before the snapshot**, such as cached random numbers, the kernel's `boot_id` and its TCP secrets, and they lose open connections. `/sync` covers the clock, the kernel's randomness, the hostname and the machine ID.
- **Snapshots stay on the host that took them**, and only restore there while the Firecracker version and host kernel stay the same. Templates are warmed again by themselves after an upgrade; VMs' own snapshots can't be.
- **A snapshot's size counts its disk copy in full**, although the copy shares most of its blocks with the image.
- **One host only.** Forks share memory and disk with their parent, which only works on one machine.
- **A VM that dies after a tined restart isn't noticed straight away.** tined watches the Firecracker processes it started. One started by an earlier tined is only checked at the next restart, or when something touches it: an exec then fails, and a destroy cleans it up. A periodic liveness check would close this.
- **No IPv6 for VMs**, and outbound traffic is otherwise unrestricted (no egress allow-list or rate limits yet).
- **Access is all or nothing.** Anyone in the `tine` group can create, use and destroy every VM. There are no per-user quotas or ownership.
- **Destroy kills the guest** without asking it to shut down. The disk is thrown away, so nothing is lost that matters.
- **The base image's disk is a fixed 4 GiB**, and there's one image so far.
