# tine's API

tined serves JSON over HTTP on a Unix socket, `/run/tine/tine.sock` (mode 0660, group `tine`). The `tine` command, the Go client in `internal/api` and the Python SDK in `sdk/python` all use it; so can `curl`:

```bash
curl --unix-socket /run/tine/tine.sock http://tined/v1/vms
```

From another machine, nothing is opened: `tine --host ssh://user@server` runs `ssh server tine dial-stdio` for each connection, and the Python SDK opens an SSH tunnel to the socket.

## Conventions

- Request and response bodies are JSON, except file contents and exec output (below).
- Times are RFC 3339, in UTC. Sizes are bytes unless a field says otherwise (`memory_mib`).
- Every response has an `X-Request-Id` header, which tined's logs carry; send one and tined uses yours.
- Most results carry `timings_ms`, how long each stage took, in milliseconds.
- An error is an HTTP status with this body. `code` is stable, for programs; `message` is for people.

  ```json
  {"error": {"code": "no_memory", "message": "not enough memory: 100 more VMs of 1024 MiB would commit ..."}}
  ```

| Code | Status | Meaning |
|---|---|---|
| `invalid_request` | 400 | The request is malformed or impossible, like a VM of 3 vCPUs. |
| `not_found`, `image_not_found` | 404 | No such VM, snapshot or image. |
| `file_not_found` | 404 | No such file in the VM. |
| `not_running` | 409 | The VM isn't running. |
| `busy` | 409 | The VM is busy: a snapshot waits for running commands and file transfers. |
| `in_use` | 409 | A snapshot that VMs run from can't be deleted. |
| `incompatible` | 409 | A snapshot this host can't restore (another Firecracker, CPU or kernel). |
| `conflict` | 409 | The request conflicts with the VM's state. |
| `capacity`, `no_memory`, `no_cpu` | 503 | Admission control: no room for the VMs asked for. The message has the numbers. |
| `no_space` | 507 | Not enough disk for a snapshot. |
| `internal` | 500 | Something broke in tined; its log has the request ID. |

## Health and metrics

| | |
|---|---|
| `GET /healthz` | `{"status": "ok", "version", "vms", "uptime_seconds", "features"}`. `features` lists the optional behaviours tined runs with, such as `"diff-snapshots"`. |
| `GET /metrics` | Prometheus metrics, also on `--metrics-listen` (see [architecture.md](architecture.md#observability)). |

## VMs

### Create: `POST /v1/vms` → 201 VM

```json
{"image": "base", "vcpus": 2, "memory_mib": 1024, "ttl_seconds": 3600, "cold": false, "strategy": "share"}
```

Every field is optional. A VM is restored from its image's template when there's one of its size (`source: "template"`), and booted otherwise (`"cold"`); `cold: true` always boots. `strategy` is how a restored VM gets its memory, as for a fork (below). vCPUs are 1 or an even number up to 32.

### The VM

```json
{
  "id": "vm-yxr4tliv4w", "state": "running", "image": "base", "vcpus": 2, "memory_mib": 1024,
  "created_at": "2026-11-08T01:02:03Z", "expires_at": "2026-11-08T02:02:03Z",
  "source": "template", "snapshot_id": "snap-xzabethmwm", "parent_id": "",
  "memory": {"rss_bytes": 133169152, "pss_bytes": 20185088, "private_bytes": 19922944, "shared_bytes": 113246208},
  "timings_ms": {"allocate": 0.4, "network": 5.0, "jail": 0.4, "start": 0.2, "restore": 12.5, "agent": 20.3, "sync": 23.3, "total": 62.4}
}
```

`state` is `creating`, `running`, `failed` (with `error` saying why) or `destroyed`. `memory` is the Firecracker process's memory on the host, for running VMs: RSS counts every page it has, PSS divides shared pages between the VMs sharing them.

| | |
|---|---|
| `GET /v1/vms` | `{"vms": [...]}`, the live VMs. `?all=true` adds destroyed ones still on record; `?memory=true` adds each one's `memory`. |
| `GET /v1/vms/{id}` | One VM, with `memory`. |
| `DELETE /v1/vms/{id}` | Destroys it, and answers with it. Destroying a destroyed VM is fine. |

### Run a command: `POST /v1/vms/{id}/exec`

```json
{"args": ["sh", "-c", "pytest -q"], "env": ["CI=1"], "dir": "/root/repo", "stdin": "<base64>", "timeout_ms": 60000, "max_output_bytes": 1048576}
```

`args` isn't run through a shell. The answer streams as the command runs, as newline-delimited JSON (`application/x-ndjson`): output frames, then one exit frame.

```
{"stream": "stdout", "data": "<base64>"}
{"stream": "stderr", "data": "<base64>"}
{"exit": {"code": 1, "signal": "", "timed_out": false, "output_truncated": false, "duration_ms": 812.5}}
```

A stream that ends without an exit frame was cut off. Closing the connection kills the command, and everything it started.

### Files

| | |
|---|---|
| `GET /v1/vms/{id}/files?path=/root/out.json` | The file's bytes, with its permissions in the `X-Tine-File-Mode` header. |
| `PUT /v1/vms/{id}/files?path=/root/run.sh&mode=0755` | Writes the body to the file, creating its directories, and answers `{"path", "size", "mode"}`. The file is replaced whole. |
| `PUT /v1/vms/{id}/archive?path=/root/repo` | Unpacks the body, a tar stream (gzipped or not), into the directory, which it creates. Answers `{"files", "dirs", "links", "bytes"}`. Entries must stay inside the directory. |

Paths are absolute. Like a command, a transfer holds off snapshots until it's done.

## Snapshots, rollback and forks

| | |
|---|---|
| `POST /v1/vms/{id}/snapshots` | Takes a snapshot of the VM, pausing it for a moment, and answers 201 with it. |
| `GET /v1/vms/{id}/snapshots` | The VM's snapshots. |
| `GET /v1/snapshots` | `{"snapshots": [...]}`, every snapshot. `?vm=`, `?kind=` (`vm`, `template`, `fork`) and `?image=` filter. |
| `GET /v1/snapshots/{id}` | One, with `used_by`: the VMs running from it. |
| `DELETE /v1/snapshots/{id}` | Deletes it, unless VMs run from it (`in_use`). |
| `POST /v1/vms/{id}/rollback` | `{"snapshot_id": "snap-..."}`: puts the VM back to one of its snapshots, or the template it started from. It keeps its ID, network and identity. |

A snapshot:

```json
{"id": "snap-u3iqp7pu3w", "kind": "vm", "state": "ready", "vm_id": "vm-yxr4tliv4w", "image": "base",
 "vcpus": 2, "memory_mib": 1024, "size_bytes": 1395864371, "created_at": "...", "used_by": [],
 "firecracker_version": "1.17.0", "timings_ms": {"paused": 157.6, "memory": 156.1, "total": 161.3}}
```

### Fork: `POST /v1/vms/{id}/fork` → 201

```json
{"count": 10, "ttl_seconds": 3600, "strategy": "share"}
```

Snapshots the VM and restores `count` children from the snapshot (1 to `--max-fork-children`, 100 by default). The answer is 201 if any child was made:

```json
{"snapshot": {...}, "children": [{"vm": {...}}, {"error": {"code": "...", "message": "..."}}],
 "timings_ms": {"snapshot": 66.1, "paused": 63.4, "children": 334.3, "total": 400.6}}
```

| `strategy` | Each child's memory |
|---|---|
| `share` (the default) | Maps the snapshot's memory file: one copy in the host's page cache for every child, until a child writes a page. |
| `copy` | Its own full copy of the memory file. The naive way, to compare against. |
| `uffd` | Served by a page server, a page at a time as the guest touches it, into memory of the child's own. |
| `uffd-prefetch` | As `uffd`, and the pages the first child touched as it started are copied in for the others before they ask. |

## Images and templates

| | |
|---|---|
| `GET /v1/images` | `{"images": [...]}`: each with its kernel, disk, agent version and templates. |
| `POST /v1/images/{name}/warm` | `{"vcpus": 2, "memory_mib": 1024}`: boots the image, waits for it to settle, and saves a template of that size. Answers 201 with the snapshot. |

Images are built on the host, with `sudo tined image import` (from a squashfs) or `sudo tined image build` (from a Dockerfile); see [architecture.md](architecture.md#images-and-disks).
