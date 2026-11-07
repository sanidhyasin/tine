"""One agent attempt: a tool-use loop with Claude, working in one sandbox."""

from __future__ import annotations

import time
from dataclasses import dataclass, field
from typing import Any

import anthropic
from tine import AsyncSandbox, TineError

DEFAULT_MODEL = "claude-sonnet-5-5"
REPO = "/root/repo"

# Dollars per million tokens: input, output, cache write (5 minutes), cache read.
PRICES = {
    "claude-sonnet-5-5": (2.00, 10.00, 2.50, 0.20),
    "claude-opus-5-5": (4.00, 20.00, 5.00, 0.20),
    "claude-haiku-5-5": (0.10, 0.50, 0.125, 0.01),
}

SYSTEM = f"""You fix bugs in a small Python repository at {REPO}, inside a Linux sandbox.

Use the tools to read the code, change it, and run the tests (`python3 -m pytest -q` in {REPO}).
Fix the bug in general, not just for the visible test: other tests, which you can't see, check more cases.
Keep the change small and in the module that has the bug; don't change the tests.
When the fix is done and the tests pass, call `done` with a one-line summary."""

TOOLS: list[dict[str, Any]] = [
    {
        "name": "run",
        "description": f"Run a shell command in {REPO} (bash), and get its exit code and output.",
        "strict": True,
        "input_schema": {
            "type": "object",
            "properties": {"command": {"type": "string", "description": "The command, e.g. `python3 -m pytest -q`."}},
            "required": ["command"],
            "additionalProperties": False,
        },
    },
    {
        "name": "read_file",
        "description": "Read a file. Paths are absolute, or relative to the repository.",
        "strict": True,
        "input_schema": {
            "type": "object",
            "properties": {"path": {"type": "string"}},
            "required": ["path"],
            "additionalProperties": False,
        },
    },
    {
        "name": "write_file",
        "description": "Write a whole file, replacing what's there. Paths are absolute, or relative to the repository.",
        "strict": True,
        "input_schema": {
            "type": "object",
            "properties": {"path": {"type": "string"}, "content": {"type": "string"}},
            "required": ["path", "content"],
            "additionalProperties": False,
        },
    },
    {
        "name": "done",
        "description": "Finish: the bug is fixed and the tests pass.",
        "strict": True,
        "input_schema": {
            "type": "object",
            "properties": {"summary": {"type": "string", "description": "One line: what was wrong, and the fix."}},
            "required": ["summary"],
            "additionalProperties": False,
        },
    },
]


@dataclass
class Usage:
    """Tokens and dollars, for one attempt or all of them."""

    input: int = 0
    output: int = 0
    cache_write: int = 0
    cache_read: int = 0
    requests: int = 0

    def add(self, u: Any) -> None:
        self.input += u.input_tokens or 0
        self.output += u.output_tokens or 0
        self.cache_write += getattr(u, "cache_creation_input_tokens", 0) or 0
        self.cache_read += getattr(u, "cache_read_input_tokens", 0) or 0
        self.requests += 1

    def merge(self, other: Usage) -> None:
        self.input += other.input
        self.output += other.output
        self.cache_write += other.cache_write
        self.cache_read += other.cache_read
        self.requests += other.requests

    def cost(self, model: str) -> float:
        pin, pout, pw, pr = PRICES.get(model, PRICES[DEFAULT_MODEL])
        return (self.input * pin + self.output * pout + self.cache_write * pw + self.cache_read * pr) / 1e6


@dataclass
class Attempt:
    """What one attempt did."""

    index: int
    steps: int = 0
    finished: bool = False
    summary: str = ""
    stop: str = ""
    seconds: float = 0.0
    usage: Usage = field(default_factory=Usage)
    error: str = ""


class Budget:
    """Stops new requests once all attempts together have spent this much."""

    def __init__(self, dollars: float, model: str) -> None:
        self.dollars, self.model = dollars, model
        self.usage = Usage()

    @property
    def spent(self) -> float:
        return self.usage.cost(self.model)

    def exhausted(self) -> bool:
        return self.spent >= self.dollars


def _path(p: str) -> str:
    return p if p.startswith("/") else f"{REPO}/{p}"


def _clip(s: str, limit: int = 12_000) -> str:
    if len(s) <= limit:
        return s
    return s[: limit // 2] + f"\n... [{len(s) - limit} characters cut] ...\n" + s[-limit // 2 :]


async def run_tool(sb: AsyncSandbox, name: str, args: dict[str, Any]) -> tuple[str, bool]:
    """Run one tool call in the sandbox: its result, and whether it failed."""
    try:
        if name == "run":
            r = await sb.exec(["bash", "-lc", args["command"]], cwd=REPO, timeout=120)
            out = (r.stdout + r.stderr).strip()
            note = " (timed out after 120 s)" if r.timed_out else ""
            return _clip(f"exit code {r.exit_code}{note}\n{out}"), False
        if name == "read_file":
            return _clip(await sb.read_text(_path(args["path"])), 50_000), False
        if name == "write_file":
            info = await sb.write_file(_path(args["path"]), args["content"])
            return f"wrote {info.size} bytes to {info.path}", False
        return f"unknown tool {name}", True
    except TineError as e:
        return f"error: {e}", True


async def attempt(
    client: anthropic.AsyncAnthropic,
    sb: AsyncSandbox,
    issue: str,
    index: int,
    *,
    model: str,
    budget: Budget,
    max_steps: int = 25,
) -> Attempt:
    """One agent's go at the bug, in its own sandbox."""
    a = Attempt(index)
    start = time.monotonic()
    listing = (await sb.exec(f"cd {REPO} && find . -path ./.git -prune -o -type f -print | sort")).stdout
    messages: list[dict[str, Any]] = [
        {"role": "user", "content": f"The issue:\n\n{issue}\n\nThe repository's files:\n{listing}"},
    ]
    try:
        while a.steps < max_steps:
            if budget.exhausted():
                a.stop = "budget"
                break
            resp = await _create(client, model, messages)
            a.steps += 1
            a.usage.add(resp.usage)
            budget.usage.add(resp.usage)
            # The whole content goes back, thinking blocks and all.
            messages.append({"role": "assistant", "content": resp.content})
            if resp.stop_reason == "refusal":
                a.stop = "refusal"
                break
            if resp.stop_reason == "pause_turn":
                continue
            calls = [b for b in resp.content if b.type == "tool_use"]
            if not calls:
                if resp.stop_reason == "end_turn":
                    messages.append(
                        {
                            "role": "user",
                            "content": "If the bug is fixed and the tests pass, call `done`; if not, carry on.",
                        }
                    )
                    continue
                a.stop = resp.stop_reason or "no tool call"
                break
            results = []
            for call in calls:
                if call.name == "done":
                    a.finished, a.summary = True, str(call.input.get("summary", ""))
                    results.append({"type": "tool_result", "tool_use_id": call.id, "content": "ok"})
                    continue
                out, failed = await run_tool(sb, call.name, call.input)
                results.append({"type": "tool_result", "tool_use_id": call.id, "content": out, "is_error": failed})
            if a.finished:
                a.stop = "done"
                break
            messages.append({"role": "user", "content": results})
        else:
            a.stop = "max steps"
    except anthropic.APIError as e:
        a.error, a.stop = f"{type(e).__name__}: {e}", "api error"
    a.seconds = time.monotonic() - start
    return a


async def _create(client: anthropic.AsyncAnthropic, model: str, messages: list[dict[str, Any]]) -> Any:
    """One turn. The system prompt and tools never change, and the
    conversation only grows, so automatic caching serves each turn's prefix
    from the cache. A refusal falls back to Anthropic's recommended model."""
    return await client.beta.messages.create(
        model=model,
        max_tokens=16000,
        system=SYSTEM,
        tools=TOOLS,
        messages=messages,
        cache_control={"type": "ephemeral"},
        output_config={"effort": "medium"},
        betas=["server-side-fallback-2026-07-01"],
        fallbacks="default",
    )
