"""Clients for tined's HTTP API, sync and async."""

from __future__ import annotations

import base64
import io
import json
import os
import tarfile
from collections.abc import Callable, Mapping, Sequence
from dataclasses import dataclass, field
from datetime import timedelta
from pathlib import Path
from typing import Any

import httpx

from ._errors import TineConnectionError, TineError, error_for
from ._models import VM, ArchiveResult, ExecResult, FileInfo, Snapshot
from ._ssh import SSHTunnel

DEFAULT_SOCKET = "/run/tine/tine.sock"

# Creates and forks take seconds, not minutes; a command can run for as long
# as it likes, so its stream has no read timeout.
_TIMEOUT = httpx.Timeout(600.0, connect=10.0)
_STREAM_TIMEOUT = httpx.Timeout(None, connect=10.0)

OutputCallback = Callable[[str, bytes], None]


@dataclass(frozen=True)
class ForkResult:
    """What a fork made: the children, the snapshot they came from, and timings."""

    snapshot: Snapshot
    children: list[VM]
    errors: list[TineError] = field(default_factory=list)
    timings_ms: dict[str, float] = field(default_factory=dict)


# --- what's shared by both clients ------------------------------------------


def _memory_mib(memory: int | str | None) -> int | None:
    """512 (MiB), "512M", "1G" or "1.5GiB" as MiB."""
    if memory is None or isinstance(memory, int):
        return memory
    s = memory.strip().upper().removesuffix("IB").removesuffix("B")
    mult = {"G": 1024, "M": 1, "T": 1024 * 1024}.get(s[-1:], None)
    if mult is None:
        return int(float(s))
    return int(float(s[:-1]) * mult)


def _seconds(ttl: float | timedelta | None) -> int | None:
    if ttl is None:
        return None
    if isinstance(ttl, timedelta):
        ttl = ttl.total_seconds()
    return int(ttl)


def _args(cmd: str | Sequence[str]) -> list[str]:
    """A string runs through sh -c; a list runs as it is."""
    if isinstance(cmd, str):
        return ["sh", "-c", cmd]
    return list(cmd)


def _exec_body(
    cmd: str | Sequence[str],
    env: Mapping[str, str] | None,
    cwd: str | None,
    stdin: str | bytes | None,
    timeout: float | None,
) -> dict[str, Any]:
    body: dict[str, Any] = {"args": _args(cmd)}
    if env:
        body["env"] = [f"{k}={v}" for k, v in env.items()]
    if cwd:
        body["dir"] = cwd
    if stdin is not None:
        data = stdin.encode() if isinstance(stdin, str) else stdin
        body["stdin"] = base64.b64encode(data).decode()
    if timeout is not None:
        body["timeout_ms"] = int(timeout * 1000)
    return body


class _Collector:
    """Gathers an exec stream's frames into an ExecResult."""

    def __init__(self, args: list[str], on_output: OutputCallback | None) -> None:
        self.args, self.on_output = args, on_output
        self.out, self.err = bytearray(), bytearray()
        self.exit: dict[str, Any] | None = None

    def line(self, line: str) -> None:
        if not line.strip():
            return
        frame = json.loads(line)
        if frame.get("exit") is not None:
            self.exit = frame["exit"]
            return
        data = base64.b64decode(frame.get("data") or "")
        stream = frame.get("stream", "stdout")
        (self.err if stream == "stderr" else self.out).extend(data)
        if self.on_output and data:
            self.on_output(stream, data)

    def result(self) -> ExecResult:
        if self.exit is None:
            raise TineError(
                "the command's output stream ended before its exit status: the connection broke", code="incomplete"
            )
        e = self.exit
        out, err = bytes(self.out), bytes(self.err)
        return ExecResult(
            args=tuple(self.args),
            exit_code=e.get("code", -1),
            stdout=out.decode(errors="replace"),
            stderr=err.decode(errors="replace"),
            duration_ms=e.get("duration_ms", 0.0),
            signal=e.get("signal", ""),
            timed_out=e.get("timed_out", False),
            output_truncated=e.get("output_truncated", False),
            stdout_bytes=out,
            stderr_bytes=err,
        )


def _raise_for(resp: httpx.Response) -> None:
    if resp.is_success:
        return
    try:
        detail = resp.json()["error"]
        code, message = detail.get("code", "unknown"), detail.get("message", "")
    except (ValueError, KeyError, TypeError):
        code, message = "unknown", resp.text.strip() or resp.reason_phrase
    raise error_for(resp.status_code, code, message)


def _create_body(
    image: str | None,
    vcpus: int | None,
    memory: int | str | None,
    ttl: float | timedelta | None,
    cold: bool,
    strategy: str | None,
    network: str | None,
) -> dict[str, Any]:
    body = {
        "image": image,
        "vcpus": vcpus,
        "memory_mib": _memory_mib(memory),
        "ttl_seconds": _seconds(ttl),
        "cold": cold or None,
        "strategy": strategy,
        "network": network,
    }
    return {k: v for k, v in body.items() if v is not None}


def _fork_body(count: int, ttl: float | timedelta | None, strategy: str | None, network: str | None) -> dict[str, Any]:
    body = {"count": count, "ttl_seconds": _seconds(ttl), "strategy": strategy, "network": network}
    return {k: v for k, v in body.items() if v is not None}


def _snapshot_params(vm: str | None, kind: str | None, image: str | None) -> dict[str, str]:
    return {k: v for k, v in (("vm", vm), ("kind", kind), ("image", image)) if v}


def _snapshot_id(snapshot: Snapshot | str) -> str:
    return snapshot.id if isinstance(snapshot, Snapshot) else snapshot


def _fork_result(d: dict[str, Any]) -> ForkResult:
    children, errors = [], []
    for c in d.get("children", []):
        if c.get("vm"):
            children.append(VM.from_json(c["vm"]))
        elif c.get("error"):
            errors.append(error_for(0, c["error"].get("code", "unknown"), c["error"].get("message", "")))
    return ForkResult(Snapshot.from_json(d["snapshot"]), children, errors, d.get("timings_ms") or {})


def _tar(local: Path) -> bytes:
    """A directory's contents as a gzipped tar, names relative to it."""
    buf = io.BytesIO()

    def anonymous(info: tarfile.TarInfo) -> tarfile.TarInfo:
        info.uid = info.gid = 0
        info.uname = info.gname = ""
        return info

    with tarfile.open(fileobj=buf, mode="w:gz") as tf:
        for child in sorted(local.iterdir()):
            tf.add(child, arcname=child.name, filter=anonymous)
    return buf.getvalue()


def _target(host: str | None, socket: str | None) -> tuple[str | None, str]:
    host = host if host is not None else os.environ.get("TINE_HOST") or None
    socket = socket or os.environ.get("TINE_SOCKET") or DEFAULT_SOCKET
    return host, socket


def _connection_error(err: Exception, where: str) -> TineConnectionError:
    return TineConnectionError(f"can't reach tined at {where}: {err}", code="unreachable")


# --- the sync client -----------------------------------------------------------


class Client:
    """A client for tined.

    By default it talks to tined on this machine, through
    ``/run/tine/tine.sock`` (or ``$TINE_SOCKET``). With ``host="ssh://user@server"``
    (or ``$TINE_HOST``), it opens an SSH tunnel to tined's socket on that
    server for as long as the client is open. Close it, or use it as a
    context manager.
    """

    def __init__(
        self, *, host: str | None = None, socket: str | None = None, transport: httpx.BaseTransport | None = None
    ) -> None:
        host, socket = _target(host, socket)
        self._tunnel: SSHTunnel | None = None
        if transport is None:
            if host:
                self._tunnel = SSHTunnel.open(host)
                socket = self._tunnel.local_socket
            transport = httpx.HTTPTransport(uds=socket)
        self._where = host or socket
        self._http = httpx.Client(transport=transport, base_url="http://tined", timeout=_TIMEOUT)

    def close(self) -> None:
        self._http.close()
        if self._tunnel:
            self._tunnel.close()

    def __enter__(self) -> Client:
        return self

    def __exit__(self, *exc: object) -> None:
        self.close()

    def _request(self, method: str, path: str, **kw: Any) -> Any:
        try:
            resp = self._http.request(method, path, **kw)
        except httpx.TransportError as e:
            raise _connection_error(e, self._where) from e
        _raise_for(resp)
        return resp.json()

    # VMs

    def create(
        self,
        image: str | None = None,
        *,
        vcpus: int | None = None,
        memory: int | str | None = None,
        ttl: float | timedelta | None = None,
        cold: bool = False,
        strategy: str | None = None,
        network: str | None = None,
    ) -> VM:
        """Create a VM, restored from its image's template if there is one (in
        about a twentieth of a second), or booted. ``memory`` is MiB or "1G".
        ``network="none"`` gives it loopback only: no internet, nothing
        beyond itself; commands and files still work."""
        body = _create_body(image, vcpus, memory, ttl, cold, strategy, network)
        return VM.from_json(self._request("POST", "/v1/vms", json=body))

    def get(self, vm_id: str) -> VM:
        return VM.from_json(self._request("GET", f"/v1/vms/{vm_id}"))

    def list(self, *, all: bool = False, memory: bool = False) -> list[VM]:
        params = {k: "true" for k, on in (("all", all), ("memory", memory)) if on}
        return [VM.from_json(v) for v in self._request("GET", "/v1/vms", params=params)["vms"]]

    def destroy(self, vm_id: str) -> VM:
        return VM.from_json(self._request("DELETE", f"/v1/vms/{vm_id}"))

    def exec(
        self,
        vm_id: str,
        cmd: str | Sequence[str],
        *,
        env: Mapping[str, str] | None = None,
        cwd: str | None = None,
        stdin: str | bytes | None = None,
        timeout: float | None = None,
        check: bool = False,
        on_output: OutputCallback | None = None,
    ) -> ExecResult:
        """Run a command in a VM and wait for it. A string runs through
        ``sh -c``; a list runs as it is. ``on_output(stream, data)`` gets
        output as it arrives. With ``check=True``, a failure raises CommandFailed."""
        body = _exec_body(cmd, env, cwd, stdin, timeout)
        col = _Collector(body["args"], on_output)
        try:
            with self._http.stream("POST", f"/v1/vms/{vm_id}/exec", json=body, timeout=_STREAM_TIMEOUT) as resp:
                if not resp.is_success:
                    resp.read()
                    _raise_for(resp)
                for line in resp.iter_lines():
                    col.line(line)
        except httpx.TransportError as e:
            raise _connection_error(e, self._where) from e
        res = col.result()
        return res.check() if check else res

    # files

    def read_file(self, vm_id: str, path: str) -> bytes:
        try:
            resp = self._http.get(f"/v1/vms/{vm_id}/files", params={"path": path}, timeout=_STREAM_TIMEOUT)
        except httpx.TransportError as e:
            raise _connection_error(e, self._where) from e
        _raise_for(resp)
        return resp.content

    def write_file(self, vm_id: str, path: str, data: str | bytes, *, mode: int = 0o644) -> FileInfo:
        content = data.encode() if isinstance(data, str) else data
        return FileInfo.from_json(
            self._request(
                "PUT",
                f"/v1/vms/{vm_id}/files",
                content=content,
                params={"path": path, "mode": f"{mode:04o}"},
                timeout=_STREAM_TIMEOUT,
            )
        )

    def upload(self, vm_id: str, local: str | os.PathLike[str], path: str) -> ArchiveResult | FileInfo:
        """Copy a local file to ``path`` in the VM, or a directory's contents
        into the directory ``path``, which is created."""
        p = Path(local)
        if p.is_dir():
            return ArchiveResult.from_json(
                self._request(
                    "PUT", f"/v1/vms/{vm_id}/archive", content=_tar(p), params={"path": path}, timeout=_STREAM_TIMEOUT
                )
            )
        return self.write_file(vm_id, path, p.read_bytes(), mode=p.stat().st_mode & 0o7777)

    # snapshots and forks

    def snapshot(self, vm_id: str, *, retain: bool = False) -> Snapshot:
        """Snapshot a running VM. A snapshot goes with its VM; with
        ``retain=True`` it's kept until ``delete_snapshot``, even once the VM
        is destroyed, to restore from later with ``fork_snapshot``."""
        body = {"retain": True} if retain else {}
        return Snapshot.from_json(self._request("POST", f"/v1/vms/{vm_id}/snapshots", json=body))

    def get_snapshot(self, snapshot: Snapshot | str) -> Snapshot:
        """A snapshot as it is now, with ``used_by``: the VMs running from it."""
        return Snapshot.from_json(self._request("GET", f"/v1/snapshots/{_snapshot_id(snapshot)}"))

    def list_snapshots(
        self, *, vm: str | None = None, kind: str | None = None, image: str | None = None
    ) -> list[Snapshot]:
        """Snapshots, oldest first: all of them, or a VM's, or of a kind
        ("vm", "template" or "fork"), or an image's."""
        resp = self._request("GET", "/v1/snapshots", params=_snapshot_params(vm, kind, image))
        return [Snapshot.from_json(s) for s in resp["snapshots"]]

    def delete_snapshot(self, snapshot: Snapshot | str) -> Snapshot:
        """Delete a snapshot. Raises Conflict while VMs run from it, and Busy
        while a fork is restoring from it."""
        return Snapshot.from_json(self._request("DELETE", f"/v1/snapshots/{_snapshot_id(snapshot)}"))

    def rollback(self, vm_id: str, snapshot_id: str) -> VM:
        return VM.from_json(self._request("POST", f"/v1/vms/{vm_id}/rollback", json={"snapshot_id": snapshot_id}))

    def fork(
        self,
        vm_id: str,
        count: int = 1,
        *,
        ttl: float | timedelta | None = None,
        strategy: str | None = None,
        network: str | None = None,
    ) -> ForkResult:
        """``count`` running copies of a VM. ``network="none"`` gives them
        loopback only; they don't inherit the parent's network."""
        return _fork_result(
            self._request("POST", f"/v1/vms/{vm_id}/fork", json=_fork_body(count, ttl, strategy, network))
        )

    def fork_snapshot(
        self,
        snapshot: Snapshot | str,
        count: int = 1,
        *,
        ttl: float | timedelta | None = None,
        strategy: str | None = None,
        network: str | None = None,
    ) -> ForkResult:
        """``count`` VMs restored from a snapshot that's already there, such
        as a retained one whose VM is gone, each starting exactly where that
        VM was. Nothing is paused. As for ``fork``, otherwise."""
        sid = _snapshot_id(snapshot)
        return _fork_result(
            self._request("POST", f"/v1/snapshots/{sid}/fork", json=_fork_body(count, ttl, strategy, network))
        )

    def health(self) -> dict[str, Any]:
        return self._request("GET", "/healthz")


# --- the async client --------------------------------------------------------


class AsyncClient:
    """Client, for asyncio: the same methods, to be awaited. Running forks'
    commands at once is the reason for it."""

    def __init__(
        self, *, host: str | None = None, socket: str | None = None, transport: httpx.AsyncBaseTransport | None = None
    ) -> None:
        host, socket = _target(host, socket)
        self._tunnel: SSHTunnel | None = None
        if transport is None:
            if host:
                self._tunnel = SSHTunnel.open(host)
                socket = self._tunnel.local_socket
            transport = httpx.AsyncHTTPTransport(uds=socket)
        self._where = host or socket
        self._http = httpx.AsyncClient(transport=transport, base_url="http://tined", timeout=_TIMEOUT)

    async def aclose(self) -> None:
        await self._http.aclose()
        if self._tunnel:
            self._tunnel.close()

    async def __aenter__(self) -> AsyncClient:
        return self

    async def __aexit__(self, *exc: object) -> None:
        await self.aclose()

    async def _request(self, method: str, path: str, **kw: Any) -> Any:
        try:
            resp = await self._http.request(method, path, **kw)
        except httpx.TransportError as e:
            raise _connection_error(e, self._where) from e
        _raise_for(resp)
        return resp.json()

    async def create(
        self,
        image: str | None = None,
        *,
        vcpus: int | None = None,
        memory: int | str | None = None,
        ttl: float | timedelta | None = None,
        cold: bool = False,
        strategy: str | None = None,
        network: str | None = None,
    ) -> VM:
        body = _create_body(image, vcpus, memory, ttl, cold, strategy, network)
        return VM.from_json(await self._request("POST", "/v1/vms", json=body))

    async def get(self, vm_id: str) -> VM:
        return VM.from_json(await self._request("GET", f"/v1/vms/{vm_id}"))

    async def list(self, *, all: bool = False, memory: bool = False) -> list[VM]:
        params = {k: "true" for k, on in (("all", all), ("memory", memory)) if on}
        return [VM.from_json(v) for v in (await self._request("GET", "/v1/vms", params=params))["vms"]]

    async def destroy(self, vm_id: str) -> VM:
        return VM.from_json(await self._request("DELETE", f"/v1/vms/{vm_id}"))

    async def exec(
        self,
        vm_id: str,
        cmd: str | Sequence[str],
        *,
        env: Mapping[str, str] | None = None,
        cwd: str | None = None,
        stdin: str | bytes | None = None,
        timeout: float | None = None,
        check: bool = False,
        on_output: OutputCallback | None = None,
    ) -> ExecResult:
        body = _exec_body(cmd, env, cwd, stdin, timeout)
        col = _Collector(body["args"], on_output)
        try:
            async with self._http.stream("POST", f"/v1/vms/{vm_id}/exec", json=body, timeout=_STREAM_TIMEOUT) as resp:
                if not resp.is_success:
                    await resp.aread()
                    _raise_for(resp)
                async for line in resp.aiter_lines():
                    col.line(line)
        except httpx.TransportError as e:
            raise _connection_error(e, self._where) from e
        res = col.result()
        return res.check() if check else res

    async def read_file(self, vm_id: str, path: str) -> bytes:
        try:
            resp = await self._http.get(f"/v1/vms/{vm_id}/files", params={"path": path}, timeout=_STREAM_TIMEOUT)
        except httpx.TransportError as e:
            raise _connection_error(e, self._where) from e
        _raise_for(resp)
        return resp.content

    async def write_file(self, vm_id: str, path: str, data: str | bytes, *, mode: int = 0o644) -> FileInfo:
        content = data.encode() if isinstance(data, str) else data
        return FileInfo.from_json(
            await self._request(
                "PUT",
                f"/v1/vms/{vm_id}/files",
                content=content,
                params={"path": path, "mode": f"{mode:04o}"},
                timeout=_STREAM_TIMEOUT,
            )
        )

    async def upload(self, vm_id: str, local: str | os.PathLike[str], path: str) -> ArchiveResult | FileInfo:
        p = Path(local)
        if p.is_dir():
            return ArchiveResult.from_json(
                await self._request(
                    "PUT", f"/v1/vms/{vm_id}/archive", content=_tar(p), params={"path": path}, timeout=_STREAM_TIMEOUT
                )
            )
        return await self.write_file(vm_id, path, p.read_bytes(), mode=p.stat().st_mode & 0o7777)

    async def snapshot(self, vm_id: str, *, retain: bool = False) -> Snapshot:
        body = {"retain": True} if retain else {}
        return Snapshot.from_json(await self._request("POST", f"/v1/vms/{vm_id}/snapshots", json=body))

    async def get_snapshot(self, snapshot: Snapshot | str) -> Snapshot:
        return Snapshot.from_json(await self._request("GET", f"/v1/snapshots/{_snapshot_id(snapshot)}"))

    async def list_snapshots(
        self, *, vm: str | None = None, kind: str | None = None, image: str | None = None
    ) -> list[Snapshot]:
        resp = await self._request("GET", "/v1/snapshots", params=_snapshot_params(vm, kind, image))
        return [Snapshot.from_json(s) for s in resp["snapshots"]]

    async def delete_snapshot(self, snapshot: Snapshot | str) -> Snapshot:
        return Snapshot.from_json(await self._request("DELETE", f"/v1/snapshots/{_snapshot_id(snapshot)}"))

    async def rollback(self, vm_id: str, snapshot_id: str) -> VM:
        return VM.from_json(await self._request("POST", f"/v1/vms/{vm_id}/rollback", json={"snapshot_id": snapshot_id}))

    async def fork(
        self,
        vm_id: str,
        count: int = 1,
        *,
        ttl: float | timedelta | None = None,
        strategy: str | None = None,
        network: str | None = None,
    ) -> ForkResult:
        return _fork_result(
            await self._request("POST", f"/v1/vms/{vm_id}/fork", json=_fork_body(count, ttl, strategy, network))
        )

    async def fork_snapshot(
        self,
        snapshot: Snapshot | str,
        count: int = 1,
        *,
        ttl: float | timedelta | None = None,
        strategy: str | None = None,
        network: str | None = None,
    ) -> ForkResult:
        sid = _snapshot_id(snapshot)
        return _fork_result(
            await self._request("POST", f"/v1/snapshots/{sid}/fork", json=_fork_body(count, ttl, strategy, network))
        )

    async def health(self) -> dict[str, Any]:
        return await self._request("GET", "/healthz")


__all__ = ["DEFAULT_SOCKET", "AsyncClient", "Client", "ForkResult", "OutputCallback"]
