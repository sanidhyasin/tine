# /// script
# requires-python = ">=3.11"
# dependencies = ["matplotlib>=3.9"]
# ///
"""Turn tine-bench results into charts and Markdown tables.

    uv run bench/analyze.py bench/results/2026-11-08-dev
    uv run bench/analyze.py --out bench/results/compare DIR_NESTED DIR_METAL

Each DIR holds a results.jsonl from tine-bench. With one DIR, the charts and
summary.md go into it. With several, --out gets charts comparing the hosts.
Warm-up runs and failed runs are left out of every number, and counted.
"""

from __future__ import annotations

import argparse
import json
import statistics
from collections import defaultdict
from pathlib import Path

import matplotlib

matplotlib.use("Agg")
import matplotlib.pyplot as plt  # noqa: E402
from matplotlib.ticker import FuncFormatter  # noqa: E402

# The reference palette (light mode), in its fixed order: a series keeps its
# color in every chart, whatever else is shown.
SURFACE, INK, INK2, MUTED, GRID, AXIS = "#fcfcfb", "#0b0b0b", "#52514e", "#898781", "#e1e0d9", "#c3c2b7"
SLOTS = ["#2a78d6", "#eb6834", "#1baf7a", "#eda100", "#e87ba4", "#008300", "#6250d6", "#e34948"]
# Fork strategies, cold boot, and anything else, each with a fixed slot.
SERIES_SLOT = {"cold": 4, "share": 0, "copy": 1, "diff": 2, "uffd": 3, "uffd-prefetch": 5}

plt.rcParams.update({
    "font.family": ["Helvetica", "Arial", "DejaVu Sans"],
    "font.size": 10,
    "axes.facecolor": SURFACE,
    "figure.facecolor": SURFACE,
    "axes.edgecolor": AXIS,
    "axes.labelcolor": INK2,
    "axes.titlecolor": INK,
    "axes.titlesize": 12,
    "axes.titleweight": "bold",
    "axes.titlelocation": "left",
    "axes.grid": True,
    "axes.axisbelow": True,
    "grid.color": GRID,
    "grid.linewidth": 0.6,
    "xtick.color": MUTED,
    "ytick.color": MUTED,
    "xtick.labelcolor": INK2,
    "ytick.labelcolor": INK2,
    "axes.spines.top": False,
    "axes.spines.right": False,
    "legend.frameon": False,
    "legend.labelcolor": INK2,
    "lines.linewidth": 2,
    "lines.markersize": 7,
    "savefig.dpi": 160,
    "savefig.bbox": "tight",
})


def color(name: str, i: int = 0) -> str:
    return SLOTS[SERIES_SLOT.get(name, 6 + i) % len(SLOTS)]


# --- loading ---------------------------------------------------------------


class Results:
    def __init__(self, directory: Path):
        self.dir = directory
        self.host: dict = {}
        self.runs: list[dict] = []
        self.skipped = defaultdict(int)  # (scenario) -> warm-up or failed runs left out
        for line in (directory / "results.jsonl").read_text().splitlines():
            if not line.strip():
                continue
            r = json.loads(line)
            if r["kind"] == "host":
                self.host = r.get("host", {})
            elif r.get("warmup") or not r.get("ok"):
                self.skipped[r.get("scenario", "?")] += 1
            else:
                self.runs.append(r)

    @property
    def label(self) -> str:
        h = self.host
        where = "bare metal" if h.get("virtualization") == "none" else "nested virtualization"
        return f"{h.get('instance_type') or h.get('hostname', '?')}, {where}"

    def points(self, scenario: str) -> dict[tuple, list[dict]]:
        """Runs of a scenario grouped by their params."""
        out: dict[tuple, list[dict]] = defaultdict(list)
        for r in self.runs:
            if r["scenario"] == scenario:
                out[tuple(sorted(r.get("params", {}).items()))].append(r)
        return out


FEATURE_NAMES = {"diff-snapshots": "diff", "huge-pages-2M": "2M"}


def features(p: dict) -> list[str]:
    """What tined and the host ran with beyond the defaults, short names."""
    return [FEATURE_NAMES.get(f, f) for f in p.get("features", "").split(",") if f]


def series(p: dict) -> str:
    """The rung a run measured: its fork strategy, and what tined ran with,
    such as "diff" for diff snapshots or "uffd+2M"."""
    parts = [p.get("strategy", "share")] + features(p)
    if parts[:2] == ["share", "diff"]:
        parts = parts[1:]  # diff snapshots are rung 4: "diff", not "share+diff"
    return "+".join(parts)


def stats(values: list[float]) -> dict:
    v = sorted(values)
    if not v:
        return {}
    at = lambda p: v[min(len(v) - 1, int(round(p * (len(v) - 1))))]  # noqa: E731
    return {"n": len(v), "p50": at(0.5), "p95": at(0.95), "p99": at(0.99), "min": v[0], "max": v[-1],
            "mean": statistics.fmean(v)}


def fmt_ms(x: float) -> str:
    if x == 0:
        return "0"
    if x >= 1000:
        return f"{x / 1000:.2f} s"
    if x >= 10:
        return f"{x:.0f} ms"
    return f"{x:.1f} ms"


def timing(runs: list[dict], key: str) -> list[float]:
    return [r["timings_ms"][key] for r in runs if key in r.get("timings_ms", {})]


# --- tables ----------------------------------------------------------------

SUMMARY_KEYS = {
    "create-cold": ["total", "client", "destroy"],
    "create-template": ["total", "client", "destroy"],
    "create-cold-cold-cache": ["total"],
    "create-template-cold-cache": ["total"],
    "exec": ["client", "guest"],
    "rollback": ["rollback"],
    "snapshot": ["paused", "total", "wait"],
    # "first": uffd-prefetch's first child, which records the pages a restore touches.
    "fork": ["total", "paused", "first", "children", "child_p50"],
}


def summary_md(res: Results) -> str:
    h = res.host
    lines = [f"# Results: {res.label}", "",
             "| Host | |", "|---|---|"]
    for k, label in [("instance_type", "Instance"), ("virtualization", "Virtualization"), ("cpu_model", "CPU"),
                     ("cpus", "CPUs"), ("memory_mib", "Memory (MiB)"), ("kernel", "Kernel"),
                     ("firecracker", "Firecracker"), ("tine", "tine"), ("data_disk", "Data disk"),
                     ("vm_dirty_ratio", "vm.dirty_ratio"), ("transparent_hugepages", "THP"), ("ksm_run", "KSM")]:
        lines.append(f"| {label} | {h.get(k, '')} |")
    lines.append("")
    lines.append("Times are in milliseconds. Warm-up and failed runs are left out; how many is noted.")
    for scenario in sorted({r["scenario"] for r in res.runs}):
        points = res.points(scenario)
        keys = SUMMARY_KEYS.get(scenario)
        lines += ["", f"## {scenario}", ""]
        if keys is None:  # memory and counts: show every number of the first run of each point
            for params, runs in sorted(points.items()):
                r = runs[0]
                vals = {**r.get("timings_ms", {}), **{f"{k} (MiB)": v for k, v in r.get("memory_mib", {}).items()}}
                lines.append(f"- `{dict(params)}`: " + ", ".join(f"{k} {v:.1f}" for k, v in sorted(vals.items())))
            continue
        lines.append("| params | measure | n | p50 | p95 | p99 | min | max |")
        lines.append("|---|---|---|---|---|---|---|---|")
        for params, runs in sorted(points.items(), key=lambda kv: str(kv[0])):
            for key in keys:
                s = stats(timing(runs, key))
                if s:
                    lines.append(f"| {', '.join(f'{k}={v}' for k, v in params)} | {key} | {s['n']} | {s['p50']:.1f} | "
                                 f"{s['p95']:.1f} | {s['p99']:.1f} | {s['min']:.1f} | {s['max']:.1f} |")
        mem = [r for rs in points.values() for r in rs if r.get("memory_mib")]
        if mem:
            lines += ["", "| params | child RSS (MiB) | child PSS (MiB) | parent RSS (MiB) |", "|---|---|---|---|"]
            for params, runs in sorted(points.items(), key=lambda kv: str(kv[0])):
                m = [r["memory_mib"] for r in runs if r.get("memory_mib")]
                if m:
                    avg = lambda k: statistics.fmean(x[k] for x in m if k in x) if any(k in x for x in m) else float("nan")  # noqa: E731
                    lines.append(f"| {', '.join(f'{k}={v}' for k, v in params)} | {avg('child_rss'):.0f} | "
                                 f"{avg('child_pss'):.0f} | {avg('parent_rss'):.0f} |")
        touch = [r for rs in points.values() for r in rs if "touch_ns_per_page_p50" in r.get("extra", {})]
        if touch:
            lines += ["", "| params | child write after the fork (ns per 4 KiB page, p50 of children) |", "|---|---|"]
            for params, runs in sorted(points.items(), key=lambda kv: str(kv[0])):
                t = [r["extra"]["touch_ns_per_page_p50"] for r in runs if "touch_ns_per_page_p50" in r.get("extra", {})]
                if t:
                    lines.append(f"| {', '.join(f'{k}={v}' for k, v in params)} | {statistics.median(t):.0f} |")
    if res.skipped:
        lines += ["", "Left out: " + ", ".join(f"{k} {v}" for k, v in sorted(res.skipped.items()))]
    return "\n".join(lines) + "\n"


# --- charts ----------------------------------------------------------------


def finish(fig, ax, path: Path, title: str, subtitle: str = ""):
    ax.set_title(title, pad=18 if subtitle else 8)
    if subtitle:
        ax.text(0, 1.02, subtitle, transform=ax.transAxes, color=INK2, fontsize=9)
    fig.savefig(path)
    plt.close(fig)
    print(f"  {path}")


def label_end(ax, x, y, text, c):
    ax.annotate(text, (x, y), xytext=(6, 0), textcoords="offset points", va="center", color=INK2, fontsize=9)


def ms_axis(ax, axis="y"):
    f = FuncFormatter(lambda v, _: fmt_ms(v))
    (ax.yaxis if axis == "y" else ax.xaxis).set_major_formatter(f)


def chart_create_cdf(sets: list[Results], out: Path):
    fig, ax = plt.subplots(figsize=(7.5, 4))
    i = 0
    for res in sets:
        lines: dict[str, list[float]] = defaultdict(list)
        for scenario in ("create-template", "create-cold"):
            for params, runs in res.points(scenario).items():
                p = dict(params)
                name = "cold" if scenario == "create-cold" else series(p)
                lines[name] += timing(runs, "total")
        for name, v in sorted(lines.items(), key=lambda kv: (kv[0] == "cold", kv[0])):
            v.sort()
            if not v:
                continue
            ys = [(k + 1) / len(v) for k in range(len(v))]
            label = ("cold boot" if name == "cold" else f"template, {name}") if len(sets) == 1 else f"{name}, {res.label}"
            c = color(name, i) if len(sets) == 1 else SLOTS[i % len(SLOTS)]
            ax.step(v, ys, where="post", color=c, label=label)
            # Each label on its own line, at a different height, so close
            # lines' labels don't collide.
            y = 0.12 + 0.16 * (i % 5)
            label_end(ax, v[min(int(y * len(v)), len(v) - 1)], y, f"p50 {fmt_ms(v[len(v) // 2])}", c)
            i += 1
    ax.set_xscale("log")
    ms_axis(ax, "x")
    ax.set_xlabel("time to a VM that runs commands (log scale)")
    ax.set_ylabel("share of creates")
    ax.set_ylim(0, 1.02)
    ax.legend(loc="lower right")
    finish(fig, ax, out / "create-cdf.png", "Create: restored from a template, or booted",
           "Every create, one at a time; the further left, the faster")


def chart_restore_stages(res: Results, out: Path):
    # The small first stages as one; "pages" (starting a page server) and
    # "record" (saving the working set) only with the uffd strategies.
    stages = [("setup", ["allocate", "network", "jail", "start"]), ("page server", ["pages"]), ("restore", ["restore"]),
              ("agent", ["agent"]), ("sync", ["sync"]), ("record", ["record"])]
    rows = []
    for params, runs in res.points("create-template").items():
        med = [sum(statistics.median(timing(runs, k)) for k in keys if timing(runs, k)) for _, keys in stages]
        if any(med):
            rows.append((series(dict(params)), med))
    if not rows:
        return
    rows.sort(key=lambda r: sum(r[1]), reverse=True)
    fig, ax = plt.subplots(figsize=(7.5, 0.9 + 0.5 * len(rows)))
    used = set()
    for y, (name, med) in enumerate(rows):
        left = 0.0
        for i, ((stage, _), m) in enumerate(zip(stages, med)):
            if m <= 0:
                continue
            ax.barh(y, max(m - 0.4, 0.05), left=left, height=0.55, color=SLOTS[i], label=stage if stage not in used else None)
            used.add(stage)
            left += m
        ax.text(left, y, f"  {fmt_ms(left)}", va="center", color=INK2, fontsize=9)
    ax.set_yticks(range(len(rows)), [name for name, _ in rows])
    ms_axis(ax, "x")
    ax.set_xlim(0, max(sum(m) for _, m in rows) * 1.15)
    ax.grid(axis="y", visible=False)
    # Just below the axis labels, whatever the number of rows.
    ax.legend(ncol=6, loc="upper center", bbox_to_anchor=(0.5, -0.55 / (0.9 + 0.5 * len(rows)) - 0.04), fontsize=8,
              handlelength=1.2, columnspacing=1.2, frameon=False)
    finish(fig, ax, out / "restore-stages.png", "Where a create from a template spends its time",
           "Median of each stage, by how the VM gets its memory")


def chart_fork(sets: list[Results], out: Path, key: str, filename: str, title: str, subtitle: str, ylabel: str):
    fig, ax = plt.subplots(figsize=(7.5, 4))
    i = 0
    xs_all: set[int] = set()
    for res in sets:
        by_strategy: dict[str, list[tuple[int, list[float]]]] = defaultdict(list)
        for params, runs in res.points("fork").items():
            p = dict(params)
            if "touch_mib" in p or "dirty_mib" in p:
                continue
            by_strategy[series(p)].append((p["children"], timing(runs, key)))
        for strategy, pts in sorted(by_strategy.items()):
            pts = sorted((n, statistics.median(v)) for n, v in pts if v)
            if not pts:
                continue
            xs, ys = zip(*pts)
            c = color(strategy) if len(sets) == 1 else SLOTS[i % len(SLOTS)]
            name = strategy if len(sets) == 1 else f"{strategy}, {res.label}"
            ax.plot(xs, ys, marker="o", color=c, label=name)
            label_end(ax, xs[-1], ys[-1], f"{name}: {fmt_ms(ys[-1])}", c)
            xs_all.update(xs)
            i += 1
    if i == 0:
        plt.close(fig)
        return
    ms_axis(ax)
    ax.set_xticks(sorted(xs_all))
    ax.set_xlabel("children")
    ax.set_ylabel(ylabel)
    ax.set_ylim(bottom=0)
    if i > 1:  # one series needs no legend: the title names it
        ax.legend(loc="upper left")
    finish(fig, ax, out / filename, title, subtitle)


def chart_snapshot(sets: list[Results], out: Path):
    fig, ax = plt.subplots(figsize=(7.5, 4))
    lines: dict[str, list[tuple[int, float]]] = defaultdict(list)
    for res in sets:
        for params, runs in res.points("snapshot").items():
            p = dict(params)
            if not timing(runs, "paused"):
                continue
            mode = "+".join(features(p)) or "full"
            name = mode if len(sets) == 1 else f"{mode}, {res.label}"
            lines[name].append((p["memory_mib"], statistics.median(timing(runs, "paused"))))
    if not lines:
        plt.close(fig)
        return
    for i, (name, pts) in enumerate(sorted(lines.items(), key=lambda kv: kv[0] != "full")):
        pts.sort()
        xs, ys = zip(*pts)
        c = SLOTS[i % len(SLOTS)]
        ax.plot(xs, ys, marker="o", color=c, label=name)
        for x, y in pts:
            label_end(ax, x, y, fmt_ms(y), c)
    ax.set_xscale("log", base=2)
    ax.xaxis.set_major_formatter(FuncFormatter(lambda v, _: f"{v / 1024:g} GiB" if v >= 1024 else f"{v:g} MiB"))
    ms_axis(ax)
    ax.set_ylim(bottom=0)
    ax.set_xlabel("VM memory")
    ax.set_ylabel("VM paused (p50)")
    if len(lines) > 1:
        ax.legend(loc="upper left")
    finish(fig, ax, out / "snapshot-paused.png", "How long a snapshot pauses a VM",
           "Median pause, by the VM's memory size")


def chart_fork_memory(res: Results, out: Path):
    rows = []
    for params, runs in res.points("fork").items():
        p = dict(params)
        if "touch_mib" in p or "dirty_mib" in p:
            continue
        m = [r["memory_mib"] for r in runs if r.get("memory_mib", {}).get("child_rss")]
        if m:
            rows.append((series(p), p["children"],
                         statistics.fmean(x["child_rss"] for x in m), statistics.fmean(x["child_pss"] for x in m)))
    if not rows:
        return
    # Top to bottom: each strategy, by number of children.
    rows.sort(key=lambda r: (r[0], -r[1]), reverse=True)
    fig, ax = plt.subplots(figsize=(7.5, 0.9 + 0.42 * len(rows)))
    h = 0.38
    for i, (_, _, rss, pss) in enumerate(rows):
        ax.barh(i + h / 2 + 0.01, rss, h, color=SLOTS[0], label="RSS" if i == 0 else None)
        ax.barh(i - h / 2 - 0.01, pss, h, color=SLOTS[1], label="PSS" if i == 0 else None)
        ax.text(rss, i + h / 2 + 0.01, f" {rss:.0f}", va="center", color=INK2, fontsize=8)
        ax.text(pss, i - h / 2 - 0.01, f" {pss:.0f}", va="center", color=INK2, fontsize=8)
    ax.set_yticks(range(len(rows)), [f"{s}, {n} {'child' if n == 1 else 'children'}" for s, n, _, _ in rows])
    ax.set_xlabel("MiB per child (mean)")
    ax.set_xlim(right=max(r[2] for r in rows) * 1.12)
    ax.grid(axis="y", visible=False)
    ax.legend(loc="upper center", bbox_to_anchor=(0.5, -0.9 / (0.9 + 0.42 * len(rows)) - 0.02), ncol=2, frameon=False)
    finish(fig, ax, out / "fork-memory.png", "What each child really costs",
           "RSS counts shared pages in full; PSS splits them between the VMs sharing them")


def chart_fork_dirty(res: Results, out: Path):
    by_strategy = defaultdict(list)
    for params, runs in res.points("fork").items():
        p = dict(params)
        if "dirty_mib" in p and timing(runs, "paused"):
            by_strategy[series(p)].append((p["dirty_mib"], statistics.median(timing(runs, "paused"))))
    if not by_strategy:
        return
    fig, ax = plt.subplots(figsize=(7.5, 4))
    for strategy, pts in sorted(by_strategy.items()):
        xs, ys = zip(*sorted(pts))
        ax.plot(xs, ys, marker="o", color=color(strategy), label=strategy)
        label_end(ax, xs[-1], ys[-1], f"{strategy}: {fmt_ms(ys[-1])}", color(strategy))
    ms_axis(ax)
    ax.set_ylim(bottom=0)
    ax.set_xlabel("memory the parent changed before the fork (MiB)")
    ax.set_ylabel("parent paused (p50)")
    ax.legend(loc="upper left")
    finish(fig, ax, out / "fork-dirty.png", "Does the pause grow with memory changed?",
           "Forking one child; the parent changed this much memory first")


def chart_fork_touch(res: Results, out: Path):
    rows = []
    for params, runs in res.points("fork").items():
        p = dict(params)
        t = [r["extra"]["touch_ns_per_page_p50"] for r in runs if "touch_ns_per_page_p50" in r.get("extra", {})]
        if "touch_mib" in p and "read" not in p and t:
            rows.append((statistics.median(t), series(p)))
    if not rows:
        return
    rows.sort()
    fig, ax = plt.subplots(figsize=(7.5, 0.6 + 0.55 * len(rows)))
    for i, (ns, strategy) in enumerate(rows):
        ax.barh(i, ns, height=0.5, color=color(strategy))
        ax.text(ns, i, f"  {ns / 1000:.1f} µs", va="center", color=INK2, fontsize=9)
    ax.set_yticks(range(len(rows)), [s for _, s in rows])
    ax.xaxis.set_major_formatter(FuncFormatter(lambda v, _: f"{v / 1000:g} µs"))
    ax.grid(axis="y", visible=False)
    ax.set_xlabel("time per 4 KiB page (median of the children)")
    finish(fig, ax, out / "fork-touch.png", "A child's first write to its parent's memory",
           "Four children, one at a time, write every page their parent filled before the fork")


def chart_fork_evict(res: Results, out: Path):
    rows = []
    for params, runs in res.points("fork").items():
        p = dict(params)
        t = [r["extra"]["touch_ns_per_page_p50"] for r in runs if "touch_ns_per_page_p50" in r.get("extra", {})]
        if p.get("read") and t:
            where = "evicted" if p.get("evicted") else "in the page cache"
            rows.append((statistics.median(t), f"{series(p)}, {where}", series(p)))
    if not rows:
        return
    rows.sort()
    fig, ax = plt.subplots(figsize=(7.5, 0.6 + 0.55 * len(rows)))
    for i, (ns, _, s) in enumerate(rows):
        ax.barh(i, ns, height=0.5, color=color(s))
        ax.text(ns, i, f"  {fmt_ns(ns)}", va="center", color=INK2, fontsize=9)
    ax.set_yticks(range(len(rows)), [label for _, label, _ in rows])
    ax.set_xscale("log")
    ax.xaxis.set_major_formatter(FuncFormatter(lambda v, _: fmt_ns(v)))
    ax.grid(axis="y", visible=False)
    ax.set_xlabel("time per 4 KiB page (median of the children, log scale)")
    finish(fig, ax, out / "fork-evict.png", "A child reading its parent's memory, before and after eviction",
           "Four children, one at a time, read every page their parent filled; evicted: the page cache was dropped first")


def fmt_ns(ns: float) -> str:
    if ns >= 1e6:
        return f"{ns / 1e6:g} ms"
    if ns >= 1e3:
        return f"{ns / 1e3:.3g} µs"
    return f"{ns:.0f} ns"


def chart_pressure(res: Results, out: Path):
    for params, runs in res.points("pressure").items():
        p = dict(params)
        rounds = runs[0].get("extra", {}).get("rounds", [])
        if not rounds:
            continue
        xs = [r["dirty_mib"] for r in rounds]
        # Two measures, two panels: never two scales on one axis.
        fig, (ax, ax2) = plt.subplots(2, 1, figsize=(7.5, 5.5), sharex=True)
        ax.plot(xs, [r["host_available_mib"] / 1024 for r in rounds], marker="o", color=SLOTS[0])
        ax.set_ylabel("host memory\navailable (GiB)")
        ax.set_ylim(bottom=0)
        ax2.plot(xs, [r["alive"] for r in rounds], marker="o", color=SLOTS[1])
        for r in rounds:
            if r.get("oom_kills"):
                ax2.annotate(f"{r['oom_kills']:.0f} OOM kills", (r["dirty_mib"], r["alive"]), textcoords="offset points",
                             xytext=(0, -14), ha="center", color=INK2, fontsize=8)
        ax2.set_ylabel("children running")
        ax2.set_ylim(bottom=0, top=p["children"] * 1.15)
        ax2.set_xlabel("memory each child has changed (MiB)")
        finish(fig, ax, out / "pressure.png", "Running the host out of memory",
               f"{p['children']} children of a {p['memory_mib'] // 1024} GiB VM, each changing {p['step_mib']} MiB more per round")
        return


def chart_idle(res: Results, out: Path):
    lines = {}
    for params, runs in res.points("idle").items():
        p = dict(params)
        samples = runs[0].get("extra", {}).get("samples", [])
        if samples:
            lines[series(p)] = (p["vms"], samples)
    if not lines:
        return
    fig, ax = plt.subplots(figsize=(7.5, 4))
    for i, (name, (n, samples)) in enumerate(sorted(lines.items())):
        xs = [s["seconds"] for s in samples]
        ys = [s.get("pss_total", 0) / n for s in samples]
        c = color(name, i)
        ax.plot(xs, ys, color=c, label=name)
        label_end(ax, xs[-1], ys[-1], f"{name}: {ys[-1]:.0f} MiB", c)
    ax.set_xlabel("seconds idle")
    ax.set_ylabel("PSS per VM (MiB)")
    ax.set_ylim(bottom=0)
    if len(lines) > 1:
        ax.legend(loc="center right")
    finish(fig, ax, out / "idle-memory.png", "What an idle VM costs the host, over time",
           f"Mean PSS of {next(iter(lines.values()))[0]} idle 256 MiB VMs, by how they were made")


def charts(sets: list[Results], out: Path):
    out.mkdir(parents=True, exist_ok=True)
    chart_create_cdf(sets, out)
    chart_snapshot(sets, out)
    chart_fork(sets, out, "total", "fork-latency.png", "Forking: total time by number of children",
               "Snapshot of the parent, then every child restored (p50)", "fork, total (p50)")
    chart_fork(sets, out, "paused", "fork-paused.png", "Forking: how long the parent is paused",
               "Only the snapshot pauses the parent (p50)", "parent paused (p50)")
    if len(sets) == 1:
        chart_restore_stages(sets[0], out)
        chart_fork_memory(sets[0], out)
        chart_fork_dirty(sets[0], out)
        chart_fork_touch(sets[0], out)
        chart_idle(sets[0], out)
        chart_fork_evict(sets[0], out)
        chart_pressure(sets[0], out)


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("dirs", nargs="+", type=Path)
    ap.add_argument("--out", type=Path, help="where comparison charts go (several DIRs)")
    a = ap.parse_args()
    sets = [Results(d) for d in a.dirs]
    if len(sets) == 1:
        res = sets[0]
        (res.dir / "summary.md").write_text(summary_md(res))
        print(f"  {res.dir / 'summary.md'}")
        charts(sets, res.dir)
    else:
        out = a.out or Path("bench/results/compare")
        charts(sets, out)


if __name__ == "__main__":
    main()
