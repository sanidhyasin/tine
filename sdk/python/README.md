# tine for Python

A Python client for [tine](https://github.com/sanidhyasin/tine), which runs Firecracker microVMs and forks running ones into many copies in milliseconds. It talks to `tined`, the tine service, on the same machine or over SSH.

```python
from tine import Sandbox

with Sandbox.create(image="python") as sb:            # destroyed at the end
    sb.exec("git clone https://github.com/... repo && pip install -e repo", check=True)
    forks = sb.fork(5)                                 # 5 running copies, already set up
    results = [f.exec("cd repo && pytest -q") for f in forks]
    print([r.ok for r in results])
```

## Install

```bash
pip install ./sdk/python        # from a checkout of tine; needs Python 3.10+ and httpx
```

## Connecting

| Where tined is | How |
|---|---|
| This machine | Nothing to set: the client uses `/run/tine/tine.sock`, or `$TINE_SOCKET`. Your user must be in the `tine` group. |
| Another machine | `host="ssh://user@server"`, or `$TINE_HOST`. The client opens an SSH tunnel to tined's socket on that server for as long as it's open, with your SSH keys and config. |

```python
from tine import Client, Sandbox

with Client(host="ssh://ubuntu@tine-dev") as c:
    with Sandbox.create(client=c) as sb:
        print(sb.exec("uname -a").stdout)
```

A `Sandbox` made without a client makes its own, and closes it when it's destroyed.

## Sandboxes

| | |
|---|---|
| `Sandbox.create(image=None, vcpus=None, memory=None, ttl=None, cold=False, strategy=None, network=None)` | A new VM, restored from its image's template in about a twentieth of a second, or booted. `memory` is MiB or `"1G"`; `ttl` is seconds or a `timedelta`. `network="none"` gives it loopback only: no internet, nothing beyond itself, while commands and files still work. |
| `Sandbox.attach(vm_id)` | One that exists already. |
| `sb.exec(cmd, env=, cwd=, stdin=, timeout=, check=, on_output=)` | Runs a command and waits. A string runs through `sh -c`, a list as it is. Returns an `ExecResult` (`exit_code`, `stdout`, `stderr`, `ok`, `duration_ms`); `check=True` raises `CommandFailed` instead of returning a failure, and `on_output(stream, data)` gets output as it arrives. |
| `sb.read_file(path)`, `sb.read_text(path)` | A file's contents. |
| `sb.write_file(path, data, mode=0o644)` | Writes a file, creating its directories. |
| `sb.upload(local, path)` | A local file to `path`, or a directory's contents into the directory `path`. |
| `sb.fork(n, ttl=None, strategy=None, network=None)` | `n` running copies, each starting exactly where this one is. They're destroyed with it. They get the default network unless `network="none"`, whatever this one has. |
| `sb.snapshot(retain=False)`, `sb.rollback(snapshot)` | A save point, and going back to it. A snapshot goes with its sandbox; with `retain=True` it's kept until it's deleted (below). |
| `Sandbox.restore(snapshot, n=1, client=, ttl=None, strategy=None, network=None)` | `n` new sandboxes restored from a snapshot, each starting where it was taken. They share `client`, which stays yours to close. |
| `sb.destroy()` | Destroys it, and its forks. A `with` block does it for you. |

## Checkpoints

A retained snapshot outlives its sandbox: take one before each step of an agent, and any step can be restored later, as many times as you like, offline if you want.

```python
from tine import Client, Sandbox

with Client(host="ssh://ubuntu@tine-dev") as c:
    with Sandbox.create(client=c) as sb:
        checkpoints = []
        for step in plan:
            checkpoints.append(sb.snapshot(retain=True))   # before each step
            sb.exec(step)
    # The sandbox is gone; its checkpoints aren't.
    retries = Sandbox.restore(checkpoints[3], 5, client=c, network="none")
    ...
    for r in retries:
        r.destroy()
    for cp in checkpoints:
        c.delete_snapshot(cp)
```

| `Client` | |
|---|---|
| `fork_snapshot(snapshot, count=1, ttl=None, strategy=None, network=None)` | VMs restored from a snapshot, as a `ForkResult`: the children, and an error for each one that couldn't be made. Nothing is paused. |
| `get_snapshot(snapshot)`, `list_snapshots(vm=None, kind=None, image=None)` | Snapshots, with `retained` and `used_by`, the VMs running from each. |
| `delete_snapshot(snapshot)` | Deletes one. `Conflict` while VMs run from it, `Busy` while it's being restored from. |

Each retained snapshot takes up to the VM's memory in disk space on the server, and a VM may have 256 of them, so delete them when a run is done.

## Async

`AsyncClient` and `AsyncSandbox` have the same methods, to be awaited. They're how to run forks' commands at once:

```python
import asyncio
from tine import AsyncSandbox

async def main():
    async with await AsyncSandbox.create(image="python") as sb:
        await sb.exec("pip install -e /root/repo", check=True)
        forks = await sb.fork(5)
        results = await asyncio.gather(*(f.exec("pytest -q", cwd="/root/repo") for f in forks))

asyncio.run(main())
```

## Errors

Everything tined refuses is a `TineError`, with tined's `code` and the HTTP `status`. The ones worth catching have their own classes: `NotFound`, `InvalidRequest`, `NotRunning`, `Busy`, `Conflict`, `CapacityError` (the host has no room), `TineConnectionError` (tined can't be reached) and `CommandFailed` (from `check=True`).

## Tests

```bash
cd sdk/python
uv run --group dev pytest                                      # against a fake tined
TINE_TEST_HOST=ssh://ubuntu@tine-dev uv run --group dev pytest  # and a real one
```

The fake runs each VM's commands in a directory of its own on your machine, so forks, files and rollbacks behave as they would.
