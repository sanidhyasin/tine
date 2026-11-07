from __future__ import annotations

import asyncio
import os
from collections.abc import Iterator
from pathlib import Path

import pytest

from fake_tined import FakeTined
from tine import (
    AsyncClient,
    AsyncSandbox,
    CapacityError,
    Client,
    CommandFailed,
    InvalidRequest,
    NotFound,
    NotRunning,
    Sandbox,
    TineConnectionError,
)
from tine._client import _memory_mib


@pytest.fixture
def fake() -> Iterator[FakeTined]:
    f = FakeTined()
    yield f
    f.close()


@pytest.fixture
def client(fake: FakeTined) -> Iterator[Client]:
    with Client(transport=fake.transport()) as c:
        yield c


def test_create_exec_destroy(client: Client, fake: FakeTined) -> None:
    sb = Sandbox.create(client=client, memory="512M", ttl=600)
    assert sb.vm.state == "running" and sb.vm.memory_mib == 512
    assert sb.vm.created_at is not None and sb.vm.created_at.microsecond == 123456

    seen: list[tuple[str, bytes]] = []
    r = sb.exec("echo out; echo err >&2; exit 3", on_output=lambda s, d: seen.append((s, d)))
    assert (r.exit_code, r.stdout, r.stderr, r.ok) == (3, "out\n", "err\n", False)
    assert ("stdout", b"out\n") in seen and ("stderr", b"err\n") in seen
    assert sb.exec(["printf", "%s", "no shell"]).stdout == "no shell"
    assert sb.exec("cat", stdin="piped").stdout == "piped"
    assert sb.exec("echo $GREETING", env={"GREETING": "hi"}).stdout == "hi\n"
    with pytest.raises(CommandFailed) as e:
        sb.exec("echo broken >&2; exit 2", check=True)
    assert e.value.result.exit_code == 2 and "broken" in str(e.value)

    sb.destroy()
    sb.destroy()  # twice is fine
    assert client.get(sb.id).state == "destroyed"
    with pytest.raises(NotRunning):
        client.exec(sb.id, "true")


def test_files(client: Client, tmp_path: Path) -> None:
    with Sandbox.create(client=client) as sb:
        info = sb.write_file("/root/run.sh", "#!/bin/sh\necho ran\n", mode=0o755)
        assert info.size == 19 and info.mode == "0755"
        assert sb.read_text("/root/run.sh").endswith("echo ran\n")
        assert sb.exec("sh root/run.sh").stdout == "ran\n"
        with pytest.raises(NotFound):
            sb.read_file("/nope")
        with pytest.raises(InvalidRequest):
            sb.write_file("relative", "x")

        repo = tmp_path / "repo"
        (repo / "pkg").mkdir(parents=True)
        (repo / "a.py").write_text("print(1)\n")
        (repo / "pkg" / "b.py").write_text("print(2)\n")
        res = sb.upload(repo, "/root/repo")
        assert res.files == 2
        assert sb.read_text("/root/repo/pkg/b.py") == "print(2)\n"
        assert sb.upload(repo / "a.py", "/root/one.py").size == 9


def test_fork_snapshot_rollback(client: Client) -> None:
    parent = Sandbox.create(client=client)
    parent.write_file("/root/state", "set up")
    forks = parent.fork(3)
    assert len(forks) == 3 and parent.fork_result.timings_ms["paused"] == 60.0
    assert all(f.vm.parent_id == parent.id for f in forks)
    for i, f in enumerate(forks):
        assert f.read_text("/root/state") == "set up"
        f.write_file("/root/state", f"child {i}")
    assert [f.read_text("/root/state") for f in forks] == ["child 0", "child 1", "child 2"]
    assert parent.read_text("/root/state") == "set up"

    snap = parent.snapshot()
    parent.write_file("/root/state", "changed")
    parent.rollback(snap)
    assert parent.read_text("/root/state") == "set up"

    with pytest.raises(CapacityError):
        parent.fork(100)
    parent.destroy()  # takes its forks with it
    assert [client.get(f.id).state for f in forks] == ["destroyed"] * 3


def test_errors(client: Client) -> None:
    with pytest.raises(NotFound) as e:
        client.get("vm-nope")
    assert e.value.code == "not_found" and e.value.status == 404
    with pytest.raises(NotFound):
        Sandbox.create(image="windows", client=client)
    with pytest.raises(CapacityError):
        client.create(memory="128G")
    with pytest.raises(TineConnectionError):
        Client(socket="/nonexistent/tine.sock").list()


def test_memory_sizes() -> None:
    assert [_memory_mib(m) for m in (512, "512M", "512MiB", "1G", "1.5GiB", "2048")] == [
        512,
        512,
        512,
        1024,
        1536,
        2048,
    ]


def test_async(fake: FakeTined) -> None:
    async def main() -> None:
        async with AsyncClient(transport=fake.transport()) as c:
            async with await AsyncSandbox.create(client=c) as sb:
                await sb.write_file("/root/n", "0")
                forks = await sb.fork(4)
                results = await asyncio.gather(*(f.exec(f"cat root/n; echo {i}") for i, f in enumerate(forks)))
                assert [r.stdout for r in results] == [f"0{i}\n" for i in range(4)]
                ids = [f.id for f in forks]
            assert [(await c.get(i)).state for i in ids] == ["destroyed"] * 4

    asyncio.run(main())


def test_ssh_host_needs_an_ssh_url() -> None:
    with pytest.raises(ValueError):
        Client(host="tine-dev")


@pytest.mark.skipif(
    not os.environ.get("TINE_TEST_HOST"), reason="set TINE_TEST_HOST=ssh://user@server to test a real tined"
)
def test_real_tined() -> None:
    with Sandbox.create(host=os.environ["TINE_TEST_HOST"], memory="512M", ttl=600) as sb:
        assert sb.exec("uname -s").stdout == "Linux\n"
        sb.write_file("/root/hello.txt", "hello from the SDK")
        forks = sb.fork(2)
        assert [f.exec("cat /root/hello.txt").stdout for f in forks] == ["hello from the SDK"] * 2
