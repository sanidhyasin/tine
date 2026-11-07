"""A fake tined, as an httpx transport, for the SDK's tests.

Each VM is a directory standing in for its root filesystem: commands run in
it (as the shell on this machine, with the directory as their working
directory), files are read and written in it, and forks, snapshots and
rollbacks copy it.
"""

from __future__ import annotations

import base64
import io
import itertools
import json
import shutil
import subprocess
import tarfile
import tempfile
import threading
from pathlib import Path
from typing import Any

import httpx


def _err(status: int, code: str, message: str) -> httpx.Response:
    return httpx.Response(status, json={"error": {"code": code, "message": message}})


class FakeTined:
    def __init__(self) -> None:
        self.root = Path(tempfile.mkdtemp(prefix="fake-tined-"))
        self.vms: dict[str, dict[str, Any]] = {}
        self.snapshots: dict[str, Path] = {}
        self._ids = itertools.count(1)
        self._lock = threading.Lock()
        self.requests: list[tuple[str, str]] = []

    def close(self) -> None:
        shutil.rmtree(self.root, ignore_errors=True)

    def transport(self) -> httpx.MockTransport:
        return httpx.MockTransport(self.handle)

    # --- helpers

    def _new_vm(
        self,
        parent: str = "",
        snapshot_id: str = "",
        source: str = "template",
        image: str = "base",
        vcpus: int = 2,
        memory_mib: int = 1024,
    ) -> dict[str, Any]:
        with self._lock:
            vid = f"vm-{next(self._ids):010d}"
        d = self.root / vid
        d.mkdir()
        vm = {
            "id": vid,
            "state": "running",
            "image": image,
            "vcpus": vcpus,
            "memory_mib": memory_mib,
            "created_at": "2026-11-08T01:02:03.123456789Z",
            "source": source,
            "snapshot_id": snapshot_id,
            "parent_id": parent,
            "timings_ms": {"total": 55.5},
        }
        self.vms[vid] = {"vm": vm, "dir": d}
        return vm

    def _vm(self, vid: str, running: bool = True) -> dict[str, Any] | httpx.Response:
        rec = self.vms.get(vid)
        if rec is None:
            return _err(404, "not_found", f"no VM {vid}")
        if running and rec["vm"]["state"] != "running":
            return _err(409, "not_running", f"VM {vid} is {rec['vm']['state']}")
        return rec

    @staticmethod
    def _in(rec: dict[str, Any], path: str) -> Path:
        return rec["dir"] / path.lstrip("/")

    # --- the API

    def handle(self, req: httpx.Request) -> httpx.Response:
        self.requests.append((req.method, req.url.path))
        parts = req.url.path.strip("/").split("/")
        q = req.url.params
        if req.url.path == "/healthz":
            return httpx.Response(200, json={"status": "ok", "version": "fake", "vms": len(self.vms)})
        if parts[:2] != ["v1", "vms"]:
            return _err(404, "not_found", "no such route")
        if len(parts) == 2:
            if req.method == "POST":
                body = json.loads(req.content or b"{}")
                if body.get("image", "base") not in ("base", "python"):
                    return _err(404, "image_not_found", f"no image {body['image']}")
                if body.get("memory_mib", 0) > 65536:
                    return _err(503, "no_memory", "not enough memory")
                vm = self._new_vm(
                    source="cold" if body.get("cold") else "template",
                    image=body.get("image", "base"),
                    vcpus=body.get("vcpus", 2),
                    memory_mib=body.get("memory_mib", 1024),
                )
                return httpx.Response(201, json=vm)
            show_all = q.get("all") == "true"
            vms = [r["vm"] for r in self.vms.values() if show_all or r["vm"]["state"] != "destroyed"]
            return httpx.Response(200, json={"vms": vms})
        rec = self._vm(parts[2], running=not (len(parts) == 3 and req.method in ("GET", "DELETE")))
        if isinstance(rec, httpx.Response):
            return rec
        if len(parts) == 3:
            if req.method == "DELETE":
                rec["vm"]["state"] = "destroyed"
            return httpx.Response(200, json=rec["vm"])
        action = parts[3]
        if action == "exec":
            return self._exec(rec, json.loads(req.content))
        if action == "files":
            path = q.get("path", "")
            if not path.startswith("/"):
                return _err(400, "bad_request", f"path {path!r} must be absolute")
            f = self._in(rec, path)
            if req.method == "GET":
                if not f.is_file():
                    return _err(404, "file_not_found", f"open {path}: no such file or directory")
                return httpx.Response(
                    200, content=f.read_bytes(), headers={"X-Tine-File-Mode": f"{f.stat().st_mode & 0o7777:04o}"}
                )
            f.parent.mkdir(parents=True, exist_ok=True)
            f.write_bytes(req.content)
            f.chmod(int(q.get("mode", "0644"), 8))
            return httpx.Response(200, json={"path": path, "size": len(req.content), "mode": q.get("mode", "0644")})
        if action == "archive":
            dest = self._in(rec, q.get("path", ""))
            dest.mkdir(parents=True, exist_ok=True)
            files = dirs = 0
            with tarfile.open(fileobj=io.BytesIO(req.content), mode="r:*") as tf:
                for m in tf.getmembers():
                    files += m.isfile()
                    dirs += m.isdir()
                tf.extractall(dest, filter="data")
            return httpx.Response(200, json={"files": files, "dirs": dirs, "links": 0, "bytes": len(req.content)})
        if action == "snapshots":
            with self._lock:
                sid = f"snap-{next(self._ids):010d}"
            shutil.copytree(rec["dir"], self.root / sid, symlinks=True)
            self.snapshots[sid] = self.root / sid
            return httpx.Response(
                201,
                json={
                    "id": sid,
                    "kind": "vm",
                    "state": "ready",
                    "vm_id": rec["vm"]["id"],
                    "image": "base",
                    "vcpus": 2,
                    "memory_mib": 1024,
                    "timings_ms": {"paused": 50.0},
                },
            )
        if action == "rollback":
            sid = json.loads(req.content)["snapshot_id"]
            if sid not in self.snapshots:
                return _err(404, "not_found", f"no snapshot {sid}")
            shutil.rmtree(rec["dir"])
            shutil.copytree(self.snapshots[sid], rec["dir"], symlinks=True)
            return httpx.Response(200, json=rec["vm"])
        if action == "fork":
            body = json.loads(req.content)
            if body.get("count", 0) > 30:
                return _err(503, "no_cpu", "not enough CPUs")
            with self._lock:
                sid = f"snap-{next(self._ids):010d}"
            children = []
            for _ in range(body["count"]):
                vm = self._new_vm(parent=rec["vm"]["id"], snapshot_id=sid, source="snapshot")
                shutil.rmtree(self.vms[vm["id"]]["dir"])
                shutil.copytree(rec["dir"], self.vms[vm["id"]]["dir"], symlinks=True)
                children.append({"vm": vm})
            return httpx.Response(
                201,
                json={
                    "snapshot": {
                        "id": sid,
                        "kind": "fork",
                        "state": "ready",
                        "vm_id": rec["vm"]["id"],
                        "image": "base",
                        "vcpus": 2,
                        "memory_mib": 1024,
                    },
                    "children": children,
                    "timings_ms": {"total": 400.0, "paused": 60.0},
                },
            )
        return _err(404, "not_found", "no such route")

    def _exec(self, rec: dict[str, Any], body: dict[str, Any]) -> httpx.Response:
        stdin = base64.b64decode(body["stdin"]) if body.get("stdin") else b""
        env = {"PATH": "/usr/bin:/bin", "HOME": str(rec["dir"])}
        env.update(kv.split("=", 1) for kv in body.get("env", []))
        cwd = self._in(rec, body["dir"]) if body.get("dir") else rec["dir"]
        timeout = body.get("timeout_ms", 0) / 1000 or None
        frames = []
        try:
            p = subprocess.run(body["args"], input=stdin, capture_output=True, cwd=cwd, env=env, timeout=timeout)
            code, timed_out = p.returncode, False
            out, err = p.stdout, p.stderr
        except subprocess.TimeoutExpired as e:
            code, timed_out = -1, True
            out, err = e.stdout or b"", e.stderr or b""
        except FileNotFoundError:
            return _err(400, "start_failed", f"{body['args'][0]}: not found")
        for stream, data in (("stdout", out), ("stderr", err)):
            if data:
                frames.append({"stream": stream, "data": base64.b64encode(data).decode()})
        frames.append({"exit": {"code": code, "timed_out": timed_out, "duration_ms": 1.5}})
        return httpx.Response(
            200,
            content="".join(json.dumps(f) + "\n" for f in frames).encode(),
            headers={"Content-Type": "application/x-ndjson"},
        )
