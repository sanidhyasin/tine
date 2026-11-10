"""What tine's API returns, as plain dataclasses."""

from __future__ import annotations

from dataclasses import dataclass, field
from datetime import datetime
from typing import Any


def _time(s: str | None) -> datetime | None:
    if not s:
        return None
    # Go writes RFC 3339 with up to nanoseconds; Python takes microseconds.
    s = s.replace("Z", "+00:00")
    if "." in s:
        head, rest = s.split(".", 1)
        digits = "".join(c for c in rest if c.isdigit())
        tz = rest[len(digits) :]
        s = f"{head}.{digits[:6]:0<6}{tz}"
    return datetime.fromisoformat(s)


@dataclass(frozen=True)
class Memory:
    """How much host memory a VM uses. PSS counts shared pages in proportion,
    so a fork child's PSS is what it really costs; RSS counts them in full."""

    rss_bytes: int
    pss_bytes: int
    private_bytes: int
    shared_bytes: int

    @classmethod
    def from_json(cls, d: dict[str, Any]) -> Memory:
        return cls(d.get("rss_bytes", 0), d.get("pss_bytes", 0), d.get("private_bytes", 0), d.get("shared_bytes", 0))


@dataclass(frozen=True)
class VM:
    """A microVM, as tined reports it."""

    id: str
    state: str
    image: str
    vcpus: int
    memory_mib: int
    created_at: datetime | None = None
    expires_at: datetime | None = None
    source: str = ""
    snapshot_id: str = ""
    parent_id: str = ""
    #: "default" (its own network, with the internet) or "none" (loopback only).
    network: str = "default"
    error: str = ""
    memory: Memory | None = None
    timings_ms: dict[str, float] = field(default_factory=dict)

    @classmethod
    def from_json(cls, d: dict[str, Any]) -> VM:
        return cls(
            id=d["id"],
            state=d.get("state", ""),
            image=d.get("image", ""),
            vcpus=d.get("vcpus", 0),
            memory_mib=d.get("memory_mib", 0),
            created_at=_time(d.get("created_at")),
            expires_at=_time(d.get("expires_at")),
            source=d.get("source", ""),
            snapshot_id=d.get("snapshot_id", ""),
            parent_id=d.get("parent_id", ""),
            network=d.get("network") or "default",
            error=d.get("error", ""),
            memory=Memory.from_json(d["memory"]) if d.get("memory") else None,
            timings_ms=d.get("timings_ms") or {},
        )


@dataclass(frozen=True)
class Snapshot:
    """A save point of a VM: its memory, devices and disk."""

    id: str
    kind: str
    state: str
    image: str
    vcpus: int
    memory_mib: int
    vm_id: str = ""
    size_bytes: int = 0
    #: Kept until it's deleted, even once its VM is destroyed.
    retained: bool = False
    created_at: datetime | None = None
    used_by: tuple[str, ...] = ()
    timings_ms: dict[str, float] = field(default_factory=dict)

    @classmethod
    def from_json(cls, d: dict[str, Any]) -> Snapshot:
        return cls(
            id=d["id"],
            kind=d.get("kind", ""),
            state=d.get("state", ""),
            image=d.get("image", ""),
            vcpus=d.get("vcpus", 0),
            memory_mib=d.get("memory_mib", 0),
            vm_id=d.get("vm_id", ""),
            size_bytes=d.get("size_bytes", 0),
            retained=bool(d.get("retained", False)),
            created_at=_time(d.get("created_at")),
            used_by=tuple(d.get("used_by") or ()),
            timings_ms=d.get("timings_ms") or {},
        )


@dataclass(frozen=True)
class ExecResult:
    """How a command ended, with its output."""

    args: tuple[str, ...]
    exit_code: int
    stdout: str
    stderr: str
    duration_ms: float = 0.0
    signal: str = ""
    timed_out: bool = False
    output_truncated: bool = False
    stdout_bytes: bytes = b""
    stderr_bytes: bytes = b""

    @property
    def ok(self) -> bool:
        """Whether the command exited with 0."""
        return self.exit_code == 0 and not self.timed_out

    def check(self) -> ExecResult:
        """Raise CommandFailed unless the command succeeded; return self."""
        from ._errors import CommandFailed

        if not self.ok:
            raise CommandFailed(self)
        return self


@dataclass(frozen=True)
class FileInfo:
    """A file written to, or read from, a VM."""

    path: str
    size: int
    mode: str

    @classmethod
    def from_json(cls, d: dict[str, Any]) -> FileInfo:
        return cls(d.get("path", ""), d.get("size", 0), d.get("mode", ""))


@dataclass(frozen=True)
class ArchiveResult:
    """What an upload of a directory unpacked."""

    files: int
    dirs: int
    links: int
    bytes: int

    @classmethod
    def from_json(cls, d: dict[str, Any]) -> ArchiveResult:
        return cls(d.get("files", 0), d.get("dirs", 0), d.get("links", 0), d.get("bytes", 0))
