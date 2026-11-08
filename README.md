# tine

Fork a running microVM into many copies in milliseconds. Built on [Firecracker](https://github.com/firecracker-microvm/firecracker).

> **Status: v0.4.0, step 4 done.** tined runs isolated microVMs, and the `tine` command creates them, runs commands in them, snapshots them, rolls them back, **forks them into many running copies that share their memory**, and destroys them. New VMs start from pre-booted templates in about a twentieth of a second. Step 4 measured every way of giving a VM its memory side by side, on nested virtualization and on bare metal, and what breaks: [docs/results.md](docs/results.md). Step 5 is a Python SDK and an AI agent demo. The step-by-step build plan is in [docs/PLAN.md](docs/PLAN.md), and the reasons behind the project are in [BRIEF.md](BRIEF.md).

## How it works

A service, `tined`, owns every VM on a Linux server. For each one it builds a jail (its own files, user, cgroup and network namespace, behind nftables rules) and starts Firecracker in it. Inside each guest, `tine-agent` runs commands for tined over vsock. Each VM's Firecracker lives in its own cgroup, so VMs keep running when tined restarts, and tined's records in SQLite let it reconcile with what's really on the host when it comes back.

A snapshot freezes a VM for a moment and saves its memory, devices and disk, read-only. A VM restored from one wakes up exactly where it left off, and tine-agent then gives it the right time, fresh randomness and its own identity. Templates are images booted once and snapshotted, so `tine create` restores instead of booting, and rollback puts a VM back to one of its snapshots.

A fork restores one snapshot of a running VM many times at once. The VM is paused only for the snapshot, and every child starts where it was, programs and all, sharing its memory until it changes a page. Admission control refuses any create or fork the host hasn't the memory or CPUs for.

Measured on the dev server (an m8i.xlarge, through nested virtualization; [results.md](docs/results.md) has bare metal too):

| What | p50 |
|---|---|
| Create from a template, to a VM that runs commands | 57 ms (41 ms through a page server with prefetch) |
| Create by booting | 1.83 s |
| Fork a 256 MiB VM into 10 running copies | 398 ms, the parent paused 64 ms (booting 10 instead: 7.5 s) |
| Fork it into 30 | 1.02 s (booting 30: 21 s) |
| Host memory for an idle fork child, one of 30 | 14 MiB (PSS), against 109 MiB for a booted VM |
| Snapshot: how long a VM is paused | 48 ms at 256 MiB, 157 ms at 1 GiB, 601 ms at 4 GiB; 4 to 6 ms with diff snapshots |
| Roll a VM back to a snapshot | 66 ms |
| Run `true` in a VM, through the API | 1.9 ms |

On bare metal (a c5d.metal), a cold boot takes 803 ms and a fork into 100 running copies 998 ms, at 12 MiB each.

The full design and the known limits are in [docs/architecture.md](docs/architecture.md); every benchmark, and what breaks, in [docs/results.md](docs/results.md).

## Layout

| Path | What it is |
|---|---|
| `BRIEF.md` | Project brief: goals, design, build order, reading list |
| `docs/PLAN.md` | The build plan: every step, its design, tasks, and "done when" checks |
| `docs/architecture.md` | How tine works: every part, snapshots and templates, the isolation, recovery, tests and measured numbers |
| `docs/results.md` | Step 4's benchmarks: every way of giving VMs their memory, measured side by side, and what breaks |
| `docs/` | Also the step-by-step walkthroughs |
| `cmd/`, `internal/` | The Go code: `tine` (CLI), `tined` (service), `tine-agent` (runs in each VM), `tine-uffd` (a page server), `tine-bench` (the benchmarks) and its workloads |
| `bench/` | `run.sh`, every benchmark in one command; `analyze.py`, the charts; `results/`, the measured runs |
| `deploy/` | `tined.service`, the systemd unit `make deploy` installs |
| `e2e/` | End-to-end tests against the real tined: `make e2e` |
| `Makefile` | `make build`, `make check`, `make deploy`, `make e2e`, `make measure`, `make bench-all`. Run `make` to list them all. |
| `scripts/dev-server.sh` | Create, start, stop and SSH into the AWS dev server. Runs on your Mac. |
| `scripts/host-setup.sh` | Prepares the server: KVM, the `/srv/tine` data volume, Firecracker, a guest kernel and disk image |
| `scripts/measure.sh` | Measures creates, execs, memory, snapshots and rollbacks against the running tined (`make measure`) |
| `artifacts/` | Downloaded binaries and images. Created on the server, never committed. |

## Quick start

On your Mac, from this folder, start the server (this also gives it its data volume):

```bash
./scripts/dev-server.sh up
```

Copy the repo over and prepare the server. It's safe to re-run.

```bash
./scripts/dev-server.sh setup
```

Build tine, install it on the server (tined runs under systemd), and build the `base` image VMs start from (Ubuntu with `tine-agent` inside), along with its template:

```bash
make base-image
```

Then log in:

```bash
./scripts/dev-server.sh ssh
```

## Using tine

On the server, `tine` talks to tined. No `sudo` is needed: `host-setup.sh` adds you to the `tine` group.

```
$ tine create                      # restored from base's template
vm-yxr4tliv4w  running  (restored from template snap-wuh3tqanur in 130ms: allocate 1.3ms · network 6.9ms · jail 0.4ms · start 2.2ms · restore 16ms · agent 64ms · sync 39ms)

$ tine exec yxr4 uname -r          # any unique prefix of an ID works
6.18.56+

$ tine exec yxr4 -- sh -c 'echo hi; exit 5'; echo "exit $?"
hi
exit 5                             # tine exits with the command's own code

$ tine snapshot yxr4               # a save point; the VM is paused for a moment
snap-u3iqp7pu3w  of vm-yxr4tliv4w  1.3 GiB  (paused 341ms: memory 340ms · disk 0.3ms; total 343ms)

$ tine exec yxr4 -- rm -rf /root   # oops
$ tine rollback yxr4 --to u3iq     # snapshot IDs take prefixes too
rolled back vm-yxr4tliv4w to snap-u3iqp7pu3w (127ms)

$ tine fork yxr4 -n 3              # three running copies
forked vm-yxr4tliv4w into 3 in 470ms (parent paused 232ms; snapshot 237ms · children 233ms)
  vm-bspt6ta7eb  running  (190ms)
  vm-7lkl5qqwiz  running  (178ms)
  vm-rxy2fbtdpu  running  (127ms)

$ tine ls --tree
VM                             STATE    IMAGE  MEMORY  RSS      PSS     AGE
vm-yxr4tliv4w                  running  base   1 GiB   1 GiB    1 GiB   49s
└─ snap-5b7nypfznt (3 forked)
   ├─ vm-bspt6ta7eb            running  base   1 GiB   127 MiB  36 MiB  2s
   ├─ vm-7lkl5qqwiz            running  base   1 GiB   127 MiB  36 MiB  2s
   └─ vm-rxy2fbtdpu            running  base   1 GiB   127 MiB  36 MiB  2s

$ tine destroy yxr4 bspt 7lkl rxy2
destroyed vm-yxr4tliv4w (29ms)
destroyed vm-bspt6ta7eb (31ms)
destroyed vm-7lkl5qqwiz (27ms)
destroyed vm-rxy2fbtdpu (30ms)
```

The output is from the dev server, with one VM's IDs throughout; the timings vary from run to run.

More:

- `tine create --vcpus 4 --memory 2G --ttl 30m`. VMs are destroyed automatically when their time is up, after 1 hour by default. `tine create -q` prints just the ID, for scripts.
- `tine create --cold` boots instead of restoring. A template is for one size, so other sizes boot unless you warm one: `tine image warm base --vcpus 4 --memory 2G`. `tine images` shows each image's templates. tined keeps `base` warm at the default size by itself, even after an upgrade.
- `tine fork VM -n N` makes N children; `-q` prints just their IDs, for scripts. `--strategy` picks how they get their memory: `share` (the default), `copy`, `uffd` (a page server copies each page in as it's touched) or `uffd-prefetch` (and copies in, before they ask, the pages the first child touched). `tine create --strategy` does the same for a VM restored from a template. `tine ls --tree` shows who was forked from whom, with each VM's RSS and PSS (PSS splits shared pages between the VMs sharing them, so it's what each really costs).
- `tine snapshot ls` lists snapshots and the VMs running from each, `tine snapshot inspect` shows one, and `tine snapshot rm` deletes them. A snapshot that VMs run from can't be deleted, and a destroyed VM's snapshots go by themselves.
- `tine exec` takes `-e KEY=VALUE`, `-w DIR`, `--timeout 30s`, and `-i` to pipe stdin in. Output streams as it happens, and Ctrl+C stops the command in the VM too.
- `tine inspect VM` shows everything, including how the VM started and where each step's time went. `tine version` also shows tined's version.
- Every command takes `--json`.

tined's API is plain HTTP on `/run/tine/tine.sock` (see `internal/api`), so `curl --unix-socket /run/tine/tine.sock http://tined/v1/vms` works too.

Some of what step 4 measured is off by default, as flags on `tined serve`: `--diff-snapshots` (a snapshot after a VM's first writes only the memory it changed), `--balloon` (booted VMs give their free pages back), `--ksm` (identical pages are merged across VMs) and `--page-server-huge-pages 2M`. [docs/architecture.md](docs/architecture.md#running-it-systemd) has them all.

To rerun the benchmarks, see [docs/results.md](docs/results.md#rerunning-it): `bench/run.sh` on any KVM host, or `make bench-all` from a Mac against the dev server.

To boot a VM by hand instead, without tined, follow [docs/step-0-boot-one-vm.md](docs/step-0-boot-one-vm.md).

When you're done for the day, stop the server from your Mac:

```bash
./scripts/dev-server.sh stop
```

The server also stops itself after 60 minutes with no SSH connections and no running VMs.
