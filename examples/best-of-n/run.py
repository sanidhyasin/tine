"""Best-of-N bug fixing with tine: set a repository up once in a sandbox,
fork it N times, let N agents try to fix the bug at once, check each fix
against hidden tests, and keep the best.

    uv run run.py                         # every task, 5 attempts each, with Claude
    uv run run.py -n 3 --tasks roman,lru  # some tasks, 3 attempts each
    uv run run.py --scripted              # no model: a scripted stand-in, to check the pipeline
    uv run run.py --compare-setup         # also time forking against setting up N sandboxes

It talks to tined on this machine, or with --host ssh://user@server (or
$TINE_HOST) on another. Claude needs ANTHROPIC_API_KEY (or `ant auth login`);
--budget caps what all attempts together may spend.
"""

from __future__ import annotations

import argparse
import asyncio
import json
import random
import sys
import time
from dataclasses import asdict, dataclass, field
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

from tine import AsyncClient, AsyncSandbox

from agent import DEFAULT_MODEL, REPO, Attempt, Budget, Usage, attempt

HERE = Path(__file__).parent
TASKS = HERE / "tasks"


@dataclass
class Outcome:
    """One attempt, checked."""

    attempt: Attempt
    passed: bool = False
    diff: str = ""
    lines_changed: int = 0
    check_seconds: float = 0.0


@dataclass
class TaskResult:
    name: str
    setup_seconds: float
    fork_seconds: float
    attempts_seconds: float
    outcomes: list[Outcome] = field(default_factory=list)
    winner: int | None = None
    resetup_seconds: float | None = None

    @property
    def passed(self) -> int:
        return sum(o.passed for o in self.outcomes)


def load_tasks(names: list[str] | None) -> list[dict[str, Any]]:
    tasks = []
    for d in sorted(p for p in TASKS.iterdir() if p.is_dir()):
        meta = json.loads((d / "task.json").read_text())
        if names and meta["name"] not in names:
            continue
        meta["dir"] = d
        tasks.append(meta)
    if names and len(tasks) != len(names):
        sys.exit(f"no such task among {sorted(p.name for p in TASKS.iterdir() if p.is_dir())}")
    return tasks


async def set_up(sb: AsyncSandbox, task: dict[str, Any]) -> None:
    """The repository, in git so each attempt's change is a diff, and its
    tests run once, as a person setting up would."""
    await sb.upload(task["dir"] / "repo", REPO)
    await sb.exec(
        f"cd {REPO} && git init -q && git add -A && git -c user.email=demo@tine -c user.name=demo commit -qm start",
        check=True,
    )
    await sb.exec(f"cd {REPO} && python3 -m pytest -q --co", check=True)


async def check(sb: AsyncSandbox, task: dict[str, Any], a: Attempt) -> Outcome:
    """Run the hidden tests, with the visible ones, on what the attempt left."""
    start = time.monotonic()
    out = Outcome(a)
    out.diff = (await sb.exec(f"cd {REPO} && git diff -- . ':!tests'")).stdout
    stat = (await sb.exec(f"cd {REPO} && git diff --numstat -- . ':!tests'")).stdout
    out.lines_changed = sum(int(x) for line in stat.splitlines() for x in line.split()[:2] if x.isdigit())
    await sb.write_file(f"{REPO}/tests/test_hidden.py", (task["dir"] / "hidden" / "test_hidden.py").read_text())
    r = await sb.exec(f"cd {REPO} && python3 -m pytest -q -p no:cacheprovider tests", timeout=300)
    out.passed = r.ok and bool(out.diff)
    out.check_seconds = time.monotonic() - start
    return out


async def scripted_attempt(sb: AsyncSandbox, task: dict[str, Any], index: int) -> Attempt:
    """No model: what an attempt does to the sandbox, scripted. Two in three
    write the reference fix; the rest give up. It checks the pipeline, not
    an agent: its success rate means nothing."""
    a = Attempt(index)
    start = time.monotonic()
    module = task["module"]
    await sb.read_text(f"{REPO}/{module}")
    if random.Random(f"{task['name']}:{index}").random() < 2 / 3:
        await sb.write_file(f"{REPO}/{module}", (task["dir"] / "solution" / module).read_text())
        r = await sb.exec(f"cd {REPO} && python3 -m pytest -q")
        a.finished, a.summary, a.stop = r.ok, "scripted: wrote the reference fix", "done"
    else:
        a.stop, a.summary = "gave up", "scripted: gave up"
    a.steps, a.seconds = 3, time.monotonic() - start
    return a


async def run_task(
    c: AsyncClient, task: dict[str, Any], n: int, args: argparse.Namespace, budget: Budget, llm: Any
) -> TaskResult:
    t0 = time.monotonic()
    async with await AsyncSandbox.create(args.image, client=c, ttl=3600) as sb:
        await set_up(sb, task)
        t1 = time.monotonic()
        forks = await sb.fork(n)
        t2 = time.monotonic()
        if args.scripted:
            attempts = await asyncio.gather(*(scripted_attempt(f, task, i) for i, f in enumerate(forks)))
        else:
            attempts = await asyncio.gather(
                *(
                    attempt(llm, f, task["issue"], i, model=args.model, budget=budget, max_steps=args.max_steps)
                    for i, f in enumerate(forks)
                )
            )
        t3 = time.monotonic()
        outcomes = await asyncio.gather(*(check(f, task, a) for f, a in zip(forks, attempts, strict=True)))
    res = TaskResult(task["name"], t1 - t0, t2 - t1, t3 - t2, list(outcomes))
    passing = [o for o in outcomes if o.passed]
    if passing:
        res.winner = min(passing, key=lambda o: (o.lines_changed, o.attempt.seconds)).attempt.index
    return res


async def resetup_time(c: AsyncClient, task: dict[str, Any], n: int, image: str | None) -> float:
    """The other way: N fresh sandboxes, each set up from scratch, at once."""
    start = time.monotonic()

    async def one() -> None:
        async with await AsyncSandbox.create(image, client=c, ttl=600) as sb:
            await set_up(sb, task)

    await asyncio.gather(*(one() for _ in range(n)))
    return time.monotonic() - start


def report(results: list[TaskResult], n: int, args: argparse.Namespace, usage: Usage, wall: float) -> dict[str, Any]:
    print()
    print(f"{'task':14} {'passed':>7} {'winner':>7} {'lines':>6} {'setup':>7} {'fork':>7} {'attempts':>9}")
    for r in results:
        win = next((o for o in r.outcomes if o.attempt.index == r.winner), None)
        print(
            f"{r.name:14} {r.passed:>3}/{n:<3} {('#' + str(r.winner)) if win else '-':>7} "
            f"{win.lines_changed if win else '-':>6} {r.setup_seconds:6.1f}s {r.fork_seconds:6.2f}s "
            f"{r.attempts_seconds:8.1f}s"
        )
    tries = [o for r in results for o in r.outcomes]
    best_of_1 = sum(o.passed for o in tries) / max(len(tries), 1)
    best_of_n = sum(r.winner is not None for r in results) / max(len(results), 1)
    print()
    label = "scripted stand-in (checks the pipeline; the rates mean nothing)" if args.scripted else args.model
    print(f"model:       {label}")
    print(f"best of 1:   {best_of_1:.0%} of single attempts fixed their bug (hidden tests pass)")
    print(f"best of {n}:   {best_of_n:.0%} of tasks had a fix among {n} attempts")
    if not args.scripted:
        print(
            f"tokens:      {usage.input:,} in, {usage.output:,} out, {usage.cache_read:,} cache reads, "
            f"{usage.cache_write:,} cache writes, {usage.requests} requests; ${usage.cost(args.model):.2f}"
        )
    print(f"wall time:   {wall:.0f}s")
    for r in results:
        if r.resetup_seconds is not None:
            print(
                f"setup once, then fork {n}: {r.setup_seconds + r.fork_seconds:.1f}s; "
                f"set up {n} sandboxes instead: {r.resetup_seconds:.1f}s ({r.name})"
            )
    for r in results:
        win = next((o for o in r.outcomes if o.attempt.index == r.winner), None)
        if win:
            print(f"\n--- {r.name}: attempt #{win.attempt.index}: {win.attempt.summary}\n{win.diff.rstrip()}")
    return {
        "time": datetime.now(timezone.utc).isoformat(),
        "model": "scripted" if args.scripted else args.model,
        "n": n,
        "best_of_1": best_of_1,
        "best_of_n": best_of_n,
        "wall_seconds": wall,
        "usage": asdict(usage),
        "cost_usd": 0.0 if args.scripted else usage.cost(args.model),
        "tasks": [
            {
                "name": r.name,
                "setup_seconds": r.setup_seconds,
                "fork_seconds": r.fork_seconds,
                "attempts_seconds": r.attempts_seconds,
                "resetup_seconds": r.resetup_seconds,
                "winner": r.winner,
                "attempts": [
                    {
                        "index": o.attempt.index,
                        "passed": o.passed,
                        "lines_changed": o.lines_changed,
                        "steps": o.attempt.steps,
                        "stop": o.attempt.stop,
                        "seconds": o.attempt.seconds,
                        "summary": o.attempt.summary,
                        "error": o.attempt.error,
                        "cost_usd": o.attempt.usage.cost(args.model),
                    }
                    for o in r.outcomes
                ],
            }
            for r in results
        ],
    }


async def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("-n", type=int, default=5, help="attempts per task (default 5)")
    ap.add_argument("--tasks", help="comma-separated task names (default: all)")
    ap.add_argument("--model", default=DEFAULT_MODEL, help=f"Claude model (default {DEFAULT_MODEL})")
    ap.add_argument("--image", default="python", help="tine image with python3, pytest and git (default python)")
    ap.add_argument("--host", help="tined on another machine: ssh://user@server (or set TINE_HOST)")
    ap.add_argument("--max-steps", type=int, default=25, help="model turns per attempt")
    ap.add_argument("--budget", type=float, default=10.0, help="dollars all attempts may spend together (default 10)")
    ap.add_argument("--scripted", action="store_true", help="no model: scripted attempts, to check the pipeline")
    ap.add_argument("--compare-setup", action="store_true", help="also time setting up N sandboxes instead of forking")
    ap.add_argument("--out", type=Path, help="write the results as JSON here")
    args = ap.parse_args()

    tasks = load_tasks(args.tasks.split(",") if args.tasks else None)
    budget = Budget(args.budget, args.model)
    llm = None
    if not args.scripted:
        import anthropic

        llm = anthropic.AsyncAnthropic()
    start = time.monotonic()
    results = []
    async with AsyncClient(host=args.host) as c:
        for task in tasks:
            print(f"{task['name']}: setting up, forking {args.n}, {args.n} attempts...", flush=True)
            r = await run_task(c, task, args.n, args, budget, llm)
            if args.compare_setup and task is tasks[0]:
                r.resetup_seconds = await resetup_time(c, task, args.n, args.image)
            results.append(r)
            print(f"  {r.passed}/{args.n} passed the hidden tests", flush=True)
            if budget.exhausted():
                print(f"budget of ${args.budget:.2f} spent: stopping", flush=True)
                break
    out = report(results, args.n, args, budget.usage, time.monotonic() - start)
    if args.out:
        args.out.parent.mkdir(parents=True, exist_ok=True)
        args.out.write_text(json.dumps(out, indent=2) + "\n")
        print(f"\nresults in {args.out}")
    return 0


if __name__ == "__main__":
    sys.exit(asyncio.run(main()))
