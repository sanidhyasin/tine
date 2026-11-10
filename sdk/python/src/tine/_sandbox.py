"""Sandbox: one VM, with the things an agent does in it."""

from __future__ import annotations

import asyncio
import os
from collections.abc import Mapping, Sequence
from datetime import timedelta

from ._client import AsyncClient, Client, ForkResult, OutputCallback
from ._errors import NotFound, NotRunning
from ._models import VM, ArchiveResult, ExecResult, FileInfo, Snapshot


class Sandbox:
    """A running VM. Use it as a context manager, and it's destroyed at the
    end, with every sandbox forked from it.

    ::

        with Sandbox.create(image="python") as sb:
            sb.exec("git clone https://github.com/... repo && pip install -e repo", check=True)
            forks = sb.fork(5)                     # 5 running copies, already set up
            results = [f.exec("cd repo && pytest -q") for f in forks]
    """

    def __init__(self, client: Client, vm: VM, *, owns_client: bool = False) -> None:
        self.client = client
        self.vm = vm
        self._owns_client = owns_client
        self._forks: list[Sandbox] = []
        self._destroyed = False

    @classmethod
    def create(
        cls,
        image: str | None = None,
        *,
        vcpus: int | None = None,
        memory: int | str | None = None,
        ttl: float | timedelta | None = None,
        cold: bool = False,
        strategy: str | None = None,
        network: str | None = None,
        client: Client | None = None,
        host: str | None = None,
    ) -> Sandbox:
        """Create a VM. Without ``client``, the sandbox makes its own (for
        ``host``, or tined on this machine) and closes it when destroyed.
        ``network="none"`` leaves it with loopback only."""
        owns = client is None
        c = client or Client(host=host)
        try:
            vm = c.create(image, vcpus=vcpus, memory=memory, ttl=ttl, cold=cold, strategy=strategy, network=network)
        except BaseException:
            if owns:
                c.close()
            raise
        return cls(c, vm, owns_client=owns)

    @classmethod
    def restore(
        cls,
        snapshot: Snapshot | str,
        n: int = 1,
        *,
        client: Client,
        ttl: float | timedelta | None = None,
        strategy: str | None = None,
        network: str | None = None,
    ) -> list[Sandbox]:
        """``n`` sandboxes restored from a snapshot, such as one taken with
        ``snapshot(retain=True)`` whose sandbox is gone, each starting where
        it was. They share ``client``, which stays the caller's to close.
        Raises if none could be made."""
        res = client.fork_snapshot(snapshot, n, ttl=ttl, strategy=strategy, network=network)
        return [cls(client, vm) for vm in res.children]

    @classmethod
    def attach(cls, vm_id: str, *, client: Client | None = None, host: str | None = None) -> Sandbox:
        """A sandbox for a VM that already exists."""
        owns = client is None
        c = client or Client(host=host)
        return cls(c, c.get(vm_id), owns_client=owns)

    @property
    def id(self) -> str:
        return self.vm.id

    def __repr__(self) -> str:
        return f"Sandbox({self.vm.id!r}, {self.vm.state})"

    def __enter__(self) -> Sandbox:
        return self

    def __exit__(self, *exc: object) -> None:
        self.destroy()

    def refresh(self) -> VM:
        self.vm = self.client.get(self.id)
        return self.vm

    def exec(
        self,
        cmd: str | Sequence[str],
        *,
        env: Mapping[str, str] | None = None,
        cwd: str | None = None,
        stdin: str | bytes | None = None,
        timeout: float | None = None,
        check: bool = False,
        on_output: OutputCallback | None = None,
    ) -> ExecResult:
        """Run a command: a string through ``sh -c``, a list as it is."""
        return self.client.exec(
            self.id, cmd, env=env, cwd=cwd, stdin=stdin, timeout=timeout, check=check, on_output=on_output
        )

    def read_file(self, path: str) -> bytes:
        return self.client.read_file(self.id, path)

    def read_text(self, path: str) -> str:
        return self.read_file(path).decode()

    def write_file(self, path: str, data: str | bytes, *, mode: int = 0o644) -> FileInfo:
        return self.client.write_file(self.id, path, data, mode=mode)

    def upload(self, local: str | os.PathLike[str], path: str) -> ArchiveResult | FileInfo:
        """Copy a local file to ``path``, or a directory's contents into ``path``."""
        return self.client.upload(self.id, local, path)

    def snapshot(self, *, retain: bool = False) -> Snapshot:
        """A save point. With ``retain=True`` it's kept, even once this
        sandbox is destroyed, until ``client.delete_snapshot``: a checkpoint to
        restore from later with ``Sandbox.restore``."""
        return self.client.snapshot(self.id, retain=retain)

    def rollback(self, snapshot: Snapshot | str) -> None:
        sid = snapshot.id if isinstance(snapshot, Snapshot) else snapshot
        self.vm = self.client.rollback(self.id, sid)

    def fork(
        self,
        n: int = 1,
        *,
        ttl: float | timedelta | None = None,
        strategy: str | None = None,
        network: str | None = None,
    ) -> list[Sandbox]:
        """``n`` running copies of this VM, each starting exactly where it is
        now. They're destroyed with this sandbox. Raises if none could be
        made; ``fork_result`` has the details of the last fork. They have the
        default network unless ``network="none"``, whatever this one has."""
        res = self.client.fork(self.id, n, ttl=ttl, strategy=strategy, network=network)
        self.fork_result: ForkResult = res
        children = [Sandbox(self.client, vm) for vm in res.children]
        self._forks.extend(children)
        return children

    def destroy(self) -> None:
        """Destroy the VM, and the sandboxes forked from it. Already gone is fine."""
        if self._destroyed:
            return
        self._destroyed = True
        try:
            for f in self._forks:
                f.destroy()
            try:
                self.vm = self.client.destroy(self.id)
            except (NotFound, NotRunning):
                pass
        finally:
            if self._owns_client:
                self.client.close()


class AsyncSandbox:
    """Sandbox, for asyncio. ``await AsyncSandbox.create(...)``, and
    ``async with`` it; its forks run their commands at once with
    ``asyncio.gather``."""

    def __init__(self, client: AsyncClient, vm: VM, *, owns_client: bool = False) -> None:
        self.client = client
        self.vm = vm
        self._owns_client = owns_client
        self._forks: list[AsyncSandbox] = []
        self._destroyed = False

    @classmethod
    async def create(
        cls,
        image: str | None = None,
        *,
        vcpus: int | None = None,
        memory: int | str | None = None,
        ttl: float | timedelta | None = None,
        cold: bool = False,
        strategy: str | None = None,
        network: str | None = None,
        client: AsyncClient | None = None,
        host: str | None = None,
    ) -> AsyncSandbox:
        owns = client is None
        c = client or AsyncClient(host=host)
        try:
            vm = await c.create(
                image, vcpus=vcpus, memory=memory, ttl=ttl, cold=cold, strategy=strategy, network=network
            )
        except BaseException:
            if owns:
                await c.aclose()
            raise
        return cls(c, vm, owns_client=owns)

    @classmethod
    async def restore(
        cls,
        snapshot: Snapshot | str,
        n: int = 1,
        *,
        client: AsyncClient,
        ttl: float | timedelta | None = None,
        strategy: str | None = None,
        network: str | None = None,
    ) -> list[AsyncSandbox]:
        res = await client.fork_snapshot(snapshot, n, ttl=ttl, strategy=strategy, network=network)
        return [cls(client, vm) for vm in res.children]

    @property
    def id(self) -> str:
        return self.vm.id

    def __repr__(self) -> str:
        return f"AsyncSandbox({self.vm.id!r}, {self.vm.state})"

    async def __aenter__(self) -> AsyncSandbox:
        return self

    async def __aexit__(self, *exc: object) -> None:
        await self.destroy()

    async def exec(
        self,
        cmd: str | Sequence[str],
        *,
        env: Mapping[str, str] | None = None,
        cwd: str | None = None,
        stdin: str | bytes | None = None,
        timeout: float | None = None,
        check: bool = False,
        on_output: OutputCallback | None = None,
    ) -> ExecResult:
        return await self.client.exec(
            self.id, cmd, env=env, cwd=cwd, stdin=stdin, timeout=timeout, check=check, on_output=on_output
        )

    async def read_file(self, path: str) -> bytes:
        return await self.client.read_file(self.id, path)

    async def read_text(self, path: str) -> str:
        return (await self.read_file(path)).decode()

    async def write_file(self, path: str, data: str | bytes, *, mode: int = 0o644) -> FileInfo:
        return await self.client.write_file(self.id, path, data, mode=mode)

    async def upload(self, local: str | os.PathLike[str], path: str) -> ArchiveResult | FileInfo:
        return await self.client.upload(self.id, local, path)

    async def snapshot(self, *, retain: bool = False) -> Snapshot:
        return await self.client.snapshot(self.id, retain=retain)

    async def rollback(self, snapshot: Snapshot | str) -> None:
        sid = snapshot.id if isinstance(snapshot, Snapshot) else snapshot
        self.vm = await self.client.rollback(self.id, sid)

    async def fork(
        self,
        n: int = 1,
        *,
        ttl: float | timedelta | None = None,
        strategy: str | None = None,
        network: str | None = None,
    ) -> list[AsyncSandbox]:
        res = await self.client.fork(self.id, n, ttl=ttl, strategy=strategy, network=network)
        self.fork_result: ForkResult = res
        children = [AsyncSandbox(self.client, vm) for vm in res.children]
        self._forks.extend(children)
        return children

    async def destroy(self) -> None:
        if self._destroyed:
            return
        self._destroyed = True
        try:
            await asyncio.gather(*(f.destroy() for f in self._forks))
            try:
                self.vm = await self.client.destroy(self.id)
            except (NotFound, NotRunning):
                pass
        finally:
            if self._owns_client:
                await self.client.aclose()
