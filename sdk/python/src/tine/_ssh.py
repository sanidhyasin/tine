"""An SSH tunnel to tined's socket on another machine."""

from __future__ import annotations

import os
import shutil
import subprocess
import tempfile
import time
from dataclasses import dataclass
from urllib.parse import urlparse

from ._errors import TineConnectionError

_REMOTE_SOCKET = "/run/tine/tine.sock"


@dataclass
class SSHTunnel:
    """``ssh -N -L local.sock:/run/tine/tine.sock host``, for as long as a
    client is open. The server's user must be able to use tined's socket
    (be in the tine group)."""

    proc: subprocess.Popen[bytes]
    local_socket: str
    _dir: str

    @classmethod
    def open(cls, host: str, timeout: float = 15.0) -> SSHTunnel:
        u = urlparse(host)
        if u.scheme != "ssh" or not u.hostname:
            raise ValueError(f"host {host!r}: want ssh://[user@]host[:port][/socket]")
        remote = u.path if u.path not in ("", "/") else _REMOTE_SOCKET
        dest = f"{u.username}@{u.hostname}" if u.username else u.hostname
        d = tempfile.mkdtemp(prefix="tine-")
        local = os.path.join(d, "tine.sock")
        argv = [
            "ssh",
            "-N",
            "-T",
            "-o",
            "BatchMode=yes",
            "-o",
            "ExitOnForwardFailure=yes",
            "-o",
            "StreamLocalBindUnlink=yes",
            "-L",
            f"{local}:{remote}",
        ]
        if u.port:
            argv += ["-p", str(u.port)]
        argv += ["--", dest]
        proc = subprocess.Popen(argv, stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=subprocess.PIPE)
        deadline = time.monotonic() + timeout
        while not os.path.exists(local):
            if proc.poll() is not None:
                err = proc.stderr.read().decode(errors="replace").strip() if proc.stderr else ""
                shutil.rmtree(d, ignore_errors=True)
                raise TineConnectionError(
                    f"ssh to {dest} failed: {err or f'exit {proc.returncode}'}", code="unreachable"
                )
            if time.monotonic() > deadline:
                proc.kill()
                shutil.rmtree(d, ignore_errors=True)
                raise TineConnectionError(
                    f"ssh to {dest} didn't open a tunnel within {timeout:.0f}s", code="unreachable"
                )
            time.sleep(0.05)
        return cls(proc, local, d)

    def close(self) -> None:
        if self.proc.poll() is None:
            self.proc.terminate()
            try:
                self.proc.wait(timeout=5)
            except subprocess.TimeoutExpired:
                self.proc.kill()
        shutil.rmtree(self._dir, ignore_errors=True)
