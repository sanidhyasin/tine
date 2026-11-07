"""Errors tine raises."""

from __future__ import annotations

from typing import TYPE_CHECKING

if TYPE_CHECKING:
    from ._models import ExecResult


class TineError(Exception):
    """Something tined refused or couldn't do.

    ``code`` is tined's stable error code (``"not_found"``, ``"busy"``, ...),
    ``status`` the HTTP status, and the message is for people.
    """

    def __init__(self, message: str, code: str = "unknown", status: int = 0) -> None:
        super().__init__(message)
        self.message = message
        self.code = code
        self.status = status


class TineConnectionError(TineError):
    """tined couldn't be reached: it isn't running, or the socket or SSH host is wrong."""


class NotFound(TineError):
    """No such VM, snapshot, image or file."""


class InvalidRequest(TineError):
    """The request was malformed, or asked for something impossible."""


class NotRunning(TineError):
    """The VM isn't running: it's being created, has failed, or is destroyed."""


class Busy(TineError):
    """The VM is busy: a snapshot waits for running commands, for instance."""


class Conflict(TineError):
    """The request conflicts with the current state: a snapshot in use, say."""


class CapacityError(TineError):
    """The host has no room: memory, CPUs, disk, or tine's own limits."""


class CommandFailed(TineError):
    """A command exited with a non-zero code, and ``check=True`` was asked for."""

    def __init__(self, result: ExecResult) -> None:
        tail = (result.stderr or result.stdout).strip().splitlines()[-5:]
        how = f"exit code {result.exit_code}"
        if result.signal:
            how = f"killed by {result.signal}"
        if result.timed_out:
            how = "timed out"
        super().__init__(
            f"command {result.args!r} failed ({how})" + ("".join(f"\n  {line}" for line in tail)), code="command_failed"
        )
        self.result = result


_BY_CODE: dict[str, type[TineError]] = {
    "not_found": NotFound,
    "image_not_found": NotFound,
    "file_not_found": NotFound,
    "invalid_request": InvalidRequest,
    "bad_request": InvalidRequest,
    "not_running": NotRunning,
    "busy": Busy,
    "conflict": Conflict,
    "in_use": Conflict,
    "incompatible": Conflict,
    "capacity": CapacityError,
    "no_memory": CapacityError,
    "no_cpu": CapacityError,
    "no_space": CapacityError,
}


def error_for(status: int, code: str, message: str) -> TineError:
    """The exception for an error response."""
    return _BY_CODE.get(code, TineError)(message, code=code, status=status)
