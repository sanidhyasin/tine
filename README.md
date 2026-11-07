# tine

Fork a running microVM into many copies in milliseconds. Built on [Firecracker](https://github.com/firecracker-microvm/firecracker).

> **Status: v0.1.0, step 1 done.** tined runs isolated microVMs, and the `tine` command creates them, runs commands in them and destroys them. Snapshots and forks come in steps 2 and 3. The step-by-step build plan is in [docs/PLAN.md](docs/PLAN.md), and the reasons behind the project are in [BRIEF.md](BRIEF.md).

## How it works

A service, `tined`, owns every VM on a Linux server. For each one it builds a jail (its own files, user, cgroup and network namespace, behind nftables rules) and starts Firecracker in it. Inside each guest, `tine-agent` runs commands for tined over vsock. Each VM's Firecracker lives in its own cgroup, so VMs keep running when tined restarts, and tined's records in SQLite let it reconcile with what's really on the host when it comes back.

Measured on the dev server (an m8i.xlarge, through nested virtualization):

| What | Result |
|---|---|
| Create, from nothing to a VM that runs commands | 1.83 s p50, 1.89 s p99, almost all of it the guest booting |
| Run `true` in a VM, through the API | 1.7 ms p50 |
| Destroy | 32 ms p50 |
| Host memory for an idle 1 GiB VM | 127 MiB |

The full design, with the numbers and the known limits, is in [docs/architecture.md](docs/architecture.md).

## Layout

| Path | What it is |
|---|---|
| `BRIEF.md` | Project brief: goals, design, build order, reading list |
| `docs/PLAN.md` | The build plan: every step, its design, tasks, and "done when" checks |
| `docs/architecture.md` | How tine works: every part, the isolation, recovery, tests and measured numbers |
| `docs/` | Also the step-by-step walkthroughs |
| `cmd/`, `internal/` | The Go code: `tine` (CLI), `tined` (service), `tine-agent` (runs in each VM) |
| `deploy/` | `tined.service`, the systemd unit `make deploy` installs |
| `e2e/` | End-to-end tests against the real tined: `make e2e` |
| `Makefile` | `make build`, `make check`, `make deploy`, `make e2e`, `make measure`. Run `make` to list them all. |
| `scripts/dev-server.sh` | Create, start, stop and SSH into the AWS dev server. Runs on your Mac. |
| `scripts/host-setup.sh` | Prepares the server: KVM, the `/srv/tine` data volume, Firecracker, a guest kernel and disk image |
| `scripts/measure.sh` | Measures create, exec and memory per VM against the running tined (`make measure`) |
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

Build tine, install it on the server (tined runs under systemd), and build the `base` image VMs boot from (Ubuntu with `tine-agent` inside):

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
$ tine create
vm-rea7pcrcpb  running  (created in 1.95s: allocate 0.4ms · network 9.8ms · jail 11ms · start 1.8ms · configure 121ms · boot 1.80s)

$ tine exec rea7 uname -r          # any unique prefix of an ID works
6.18.56+

$ tine exec rea7 -- sh -c 'echo hi; exit 5'; echo "exit $?"
hi
exit 5                             # tine exits with the command's own code

$ tine ls
ID             STATE    IMAGE  VCPUS  MEMORY  AGE  EXPIRES IN
vm-rea7pcrcpb  running  base   2      1 GiB   3s   59m

$ tine destroy rea7
destroyed vm-rea7pcrcpb (29ms)
```

More:

- `tine create --vcpus 4 --memory 2G --ttl 30m`. VMs are destroyed automatically when their time is up, after 1 hour by default. `tine create -q` prints just the ID, for scripts.
- `tine exec` takes `-e KEY=VALUE`, `-w DIR`, `--timeout 30s`, and `-i` to pipe stdin in. Output streams as it happens, and Ctrl+C stops the command in the VM too.
- `tine inspect VM` shows everything, including where each step's time went. `tine images` and `tine version` (which also shows tined's version) round it out.
- Every command takes `--json`.

tined's API is plain HTTP on `/run/tine/tine.sock` (see `internal/api`), so `curl --unix-socket /run/tine/tine.sock http://tined/v1/vms` works too.

To boot a VM by hand instead, without tined, follow [docs/step-0-boot-one-vm.md](docs/step-0-boot-one-vm.md).

When you're done for the day, stop the server from your Mac:

```bash
./scripts/dev-server.sh stop
```

The server also stops itself after 60 minutes with no SSH connections and no running VMs.
