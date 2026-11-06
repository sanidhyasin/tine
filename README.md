# tine

Fork a running microVM into many copies in milliseconds. Built on [Firecracker](https://github.com/firecracker-microvm/firecracker).

> **Status:** step 0 (booting a VM by hand) is done; step 1 is next. The step-by-step build plan is in [docs/PLAN.md](docs/PLAN.md), and the reasons behind the project are in [BRIEF.md](BRIEF.md).

## Layout

| Path | What it is |
|---|---|
| `BRIEF.md` | Project brief: goals, design, build order, reading list |
| `docs/PLAN.md` | The build plan: every step, its design, tasks, and "done when" checks |
| `docs/` | The plan and step-by-step walkthroughs |
| `cmd/`, `internal/` | The Go code: `tine` (CLI), `tined` (service), `tine-agent` (runs in each VM) |
| `Makefile` | `make build`, `make check`, `make deploy`. Run `make` to list them all. |
| `scripts/dev-server.sh` | Create, start, stop and SSH into the AWS dev server. Runs on your Mac. |
| `scripts/host-setup.sh` | Prepares the server: KVM, the `/srv/tine` data volume, Firecracker, a guest kernel and disk image |
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

Then log in:

```bash
./scripts/dev-server.sh ssh
```

To boot a VM by hand, follow [docs/step-0-boot-one-vm.md](docs/step-0-boot-one-vm.md).

When you're done for the day, stop the server from your Mac:

```bash
./scripts/dev-server.sh stop
```

The server also stops itself after 60 minutes with no SSH connections and no running VMs.
