"""Python client for tine: fork running Firecracker microVMs in milliseconds.

::

    from tine import Sandbox

    with Sandbox.create(image="python") as sb:
        sb.exec("pip install -e /root/repo", check=True)
        forks = sb.fork(5)
        results = [f.exec("cd /root/repo && pytest -q") for f in forks]
"""

from ._client import DEFAULT_SOCKET, AsyncClient, Client, ForkResult
from ._errors import (
    Busy,
    CapacityError,
    CommandFailed,
    Conflict,
    InvalidRequest,
    NotFound,
    NotRunning,
    TineConnectionError,
    TineError,
)
from ._models import VM, ArchiveResult, ExecResult, FileInfo, Memory, Snapshot
from ._sandbox import AsyncSandbox, Sandbox

__version__ = "0.5.0"

__all__ = [
    "DEFAULT_SOCKET",
    "VM",
    "ArchiveResult",
    "AsyncClient",
    "AsyncSandbox",
    "Busy",
    "CapacityError",
    "Client",
    "CommandFailed",
    "Conflict",
    "ExecResult",
    "FileInfo",
    "ForkResult",
    "InvalidRequest",
    "Memory",
    "NotFound",
    "NotRunning",
    "Sandbox",
    "Snapshot",
    "TineConnectionError",
    "TineError",
]
