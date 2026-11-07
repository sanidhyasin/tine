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
| `Sandbox.create(image=None, vcpus=None, memory=None, ttl=None, cold=False, strategy=None)` | A new VM, restored from its image's template in about a twentieth of a second, or booted. `memory` is MiB or `"1G"`; `ttl` is seconds or a `timedelta`. |
| `Sandbox.attach(vm_id)` | One that exists already. |
| `sb.exec(cmd, env=, cwd=, stdin=, timeout=, check=, on_output=)` | Runs a command and waits. A string runs through `sh -c`, a list as it is. Returns an `ExecResult` (`exit_code`, `stdout`, `stderr`, `ok`, `duration_ms`); `check=True` raises `CommandFailed` instead of returning a failure, and `on_output(stream, data)` gets output as it arrives. |
| `sb.read_file(path)`, `sb.read_text(path)` | A file's contents. |
| `sb.write_file(path, data, mode=0o644)` | Writes a file, creating its directories. |
| `sb.upload(local, path)` | A local file to `path`, or a directory's contents into the directory `path`. |
| `sb.fork(n, ttl=None, strategy=None)` | `n` running copies, each starting exactly where this one is. They're destroyed with it. |
| `sb.snapshot()`, `sb.rollback(snapshot)` | A save point, and going back to it. |
| `sb.destroy()` | Destroys it, and its forks. A `with` block does it for you. |

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
