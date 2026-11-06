# tine

Fork a running microVM into many copies in milliseconds. Built on [Firecracker](https://github.com/firecracker-microvm/firecracker).

> **Status:** step 0, booting one VM by hand. See [BRIEF.md](BRIEF.md) for the full plan.

## Layout

| Path | What it is |
|---|---|
| `BRIEF.md` | Project brief: goals, design, build order, reading list |
| `docs/` | Step-by-step walkthroughs |
| `scripts/dev-server.sh` | Create, start, stop and SSH into the AWS dev server. Runs on your Mac. |
| `scripts/host-setup.sh` | Installs Firecracker, a guest kernel and a guest disk image. Runs on the server. |
| `artifacts/` | Downloaded binaries and images. Created on the server, never committed. |

The Go manager and the `tine` CLI arrive in step 1.

## Quick start

On your Mac, from this folder:

```bash
./scripts/dev-server.sh up
```

```bash
./scripts/dev-server.sh sync
```

```bash
./scripts/dev-server.sh ssh
```

On the server:

```bash
cd ~/tine && ./scripts/host-setup.sh
```

Then follow [docs/step-0-boot-one-vm.md](docs/step-0-boot-one-vm.md).

When you're done for the day, stop the server from your Mac:

```bash
./scripts/dev-server.sh stop
```

The server also stops itself after 60 minutes with no SSH connections and no running VMs.
