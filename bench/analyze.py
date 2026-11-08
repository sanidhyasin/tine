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
import math
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
# The rungs of the ladder, in its order. Runs with a host setting or a tined
# flag on top (share+thp, uffd+2M) are in the tables, and in charts of their own.
RUNGS = ["cold", "copy", "share", "diff", "uffd", "uffd-prefetch"]
# Direct labels for this many series at most; more get a legend alone.
MAX_LABELS = 4

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
        return f"{x / 1000:.3g} s"
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
    ax.set_title(title, pad=20 if subtitle else 8)
    if subtitle:
        # In points above the axes, so it sits under the title however tall the chart.
        ax.annotate(subtitle, (0, 1), xycoords="axes fraction", xytext=(0, 6), textcoords="offset points",
                    color=INK2, fontsize=9)
    fig.savefig(path)
    plt.close(fig)
    print(f"  {path}")


def legend_below(fig, ax, ncol: int, **kw):
    """A legend in one row under the axis label, however tall the chart."""
    axes_inches = fig.get_size_inches()[1] * ax.get_position().height
    ax.legend(loc="upper center", bbox_to_anchor=(0.5, -0.62 / axes_inches), ncol=ncol, frameon=False, **kw)


def label_end(ax, x, y, text, c):
    ax.annotate(text, (x, y), xytext=(6, 0), textcoords="offset points", va="center", color=INK2, fontsize=9)


def label_ends(ax, ends: list[tuple[float, float, str]], fmt=None):
    """Label the last point of each line, "name: value", merging lines that
    end too close together to label apart ("copy, share: 54 ms")."""
    fmt = fmt or fmt_ms
    lo, hi = ax.get_ylim()
    near = (hi - lo) * 0.05
    groups: list[list[tuple[float, float, str]]] = []
    for e in sorted(ends, key=lambda e: e[1]):
        if groups and abs(e[1] - groups[-1][-1][1]) < near:
            groups[-1].append(e)
        else:
            groups.append([e])
    for g in groups:
        x, y = max(e[0] for e in g), statistics.fmean(e[1] for e in g)
        label_end(ax, x, y, f"{', '.join(sorted(e[2] for e in g))}: {fmt(statistics.fmean(e[1] for e in g))}", None)


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
                if p.get("features"):  # with a host setting on top: in the tables
                    continue
                name = "cold" if scenario == "create-cold" else series(p)
                lines[name] += timing(runs, "total")
        for name, v in sorted(lines.items(), key=lambda kv: (kv[0] == "cold", kv[0])):
            v.sort()
            if not v:
                continue
            ys = [(k + 1) / len(v) for k in range(len(v))]
            label = ("cold boot" if name == "cold" else f"template, {name}") if len(sets) == 1 else f"{name}, {res.label}"
            c = color(name, i) if len(sets) == 1 else SLOTS[i % len(SLOTS)]
            # The p50 in the legend: the lines are too close to label.
            ax.step(v, ys, where="post", color=c, label=f"{label}: p50 {fmt_ms(v[len(v) // 2])}")
            i += 1
    ax.set_xscale("log")
    ms_axis(ax, "x")
    ax.set_xlabel("time to a VM that runs commands (log scale)")
    ax.set_ylabel("share of creates")
    ax.set_ylim(0, 1.02)
    legend_below(fig, ax, 3, fontsize=8)
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
    legend_below(fig, ax, 6, fontsize=8, handlelength=1.2, columnspacing=1.2)
    finish(fig, ax, out / "restore-stages.png", "Where a create from a template spends its time",
           "Median of each stage, by how the VM gets its memory")


def chart_fork(sets: list[Results], out: Path, key: str, filename: str, title: str, subtitle: str, ylabel: str,
               log: bool = False, rungs: list[str] = RUNGS, legend: str = "upper left"):
    fig, ax = plt.subplots(figsize=(7.5, 4))
    lines = []
    for h, res in enumerate(sets):
        by_strategy: dict[str, list[tuple[int, list[float]]]] = defaultdict(list)
        for params, runs in res.points("fork").items():
            p = dict(params)
            if "touch_mib" in p or "dirty_mib" in p or series(p) not in rungs:
                continue
            by_strategy[series(p)].append((p["children"], timing(runs, key)))
        for strategy in rungs:
            pts = sorted((n, statistics.median(v)) for n, v in by_strategy.get(strategy, []) if v)
            if pts:
                lines.append((h, strategy, res, pts))
    if not lines:
        plt.close(fig)
        return
    xs_all: set[int] = set()
    ends = []
    for h, strategy, res, pts in lines:
        xs, ys = zip(*pts)
        # One host: a color per strategy. Several: the same, and a line style per host.
        name = strategy if len(sets) == 1 else f"{strategy}, {res.label}"
        ax.plot(xs, ys, marker="o", color=color(strategy), linestyle=["-", "--", ":"][h % 3], label=name)
        ends.append((xs[-1], ys[-1], name))
        xs_all.update(xs)
    ms_axis(ax)
    ax.set_xticks(sorted(xs_all))
    ax.set_xlabel("children")
    if log:
        ax.set_yscale("log")
        low = min(y for _, _, _, pts in lines for _, y in pts)
        ax.set_ylim(bottom=10 ** math.floor(math.log10(low)))  # a labelled tick under the lowest point
        ms_axis(ax)
        ax.set_ylabel(f"{ylabel}, log scale")
    else:
        ax.set_ylim(bottom=0)
        ax.set_ylabel(ylabel)
    if len(lines) <= MAX_LABELS and not log:
        label_ends(ax, ends)
    if len(lines) > 1:  # one series needs no legend: the title names it
        ax.legend(loc=legend, fontsize=8 if len(sets) > 1 else 9)
    finish(fig, ax, out / filename, title, subtitle)


SNAPSHOT_COLORS = {"full": "share", "diff": "diff"}  # the strategy each snapshot kind goes with


def chart_snapshot(sets: list[Results], out: Path):
    fig, ax = plt.subplots(figsize=(7.5, 4))
    lines: dict[tuple[int, str], list[tuple[int, float]]] = defaultdict(list)
    for h, res in enumerate(sets):
        for params, runs in res.points("snapshot").items():
            p = dict(params)
            if timing(runs, "paused"):
                mode = "+".join(features(p)) or "full"
                lines[(h, mode)].append((p["memory_mib"], statistics.median(timing(runs, "paused"))))
    if not lines:
        plt.close(fig)
        return
    order = {"full": 0, "diff": 1}
    for (h, mode), pts in sorted(lines.items(), key=lambda kv: (order.get(kv[0][1], 2), kv[0][1], kv[0][0])):
        pts.sort()
        xs, ys = zip(*pts)
        c = color(SNAPSHOT_COLORS[mode]) if mode in SNAPSHOT_COLORS else SLOTS[6]
        name = mode if len(sets) == 1 else f"{mode}, {sets[h].label}"
        ax.plot(xs, ys, marker="o", color=c, linestyle=["-", "--", ":"][h % 3], label=name)
        label_end(ax, xs[-1], ys[-1], fmt_ms(ys[-1]), c)  # the rest are in the table
    ax.set_xscale("log", base=2)
    ax.xaxis.set_major_formatter(FuncFormatter(lambda v, _: f"{v / 1024:g} GiB" if v >= 1024 else f"{v:g} MiB"))
    ms_axis(ax)
    ax.set_ylim(bottom=0)
    ax.set_xlabel("VM memory")
    ax.set_ylabel("VM paused (p50)")
    if len(lines) > 1:
        ax.legend(loc="upper left", fontsize=8 if len(sets) > 1 else 9)
    finish(fig, ax, out / "snapshot-paused.png", "How long a snapshot pauses a VM",
           "Median pause, by the VM's memory size")


def chart_fork_memory(res: Results, out: Path):
    rows = []
    for params, runs in res.points("fork").items():
        p = dict(params)
        # The rungs, once there's something to share. Huge pages (+2M) are
        # left out: hugetlbfs memory isn't counted in RSS or PSS at all.
        if "touch_mib" in p or "dirty_mib" in p or series(p) not in RUNGS or p["children"] < 10:
            continue
        m = [r["memory_mib"] for r in runs if r.get("memory_mib", {}).get("child_rss")]
        if m:
            rows.append((series(p), p["children"],
                         statistics.fmean(x["child_rss"] for x in m), statistics.fmean(x["child_pss"] for x in m)))
    if not rows:
        return
    # Top to bottom: the rungs in order, each by number of children.
    rows.sort(key=lambda r: (RUNGS.index(r[0]), r[1]), reverse=True)
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
    legend_below(fig, ax, 2)
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
    ends = []
    for strategy in [s for s in RUNGS if s in by_strategy]:
        xs, ys = zip(*sorted(by_strategy[strategy]))
        ax.plot(xs, ys, marker="o", color=color(strategy), label=strategy)
        ends.append((xs[-1], ys[-1], strategy))
    ms_axis(ax)
    ax.set_ylim(bottom=0, top=max(e[1] for e in ends) * 1.25)
    label_ends(ax, ends)
    ax.set_xlabel("memory the parent changed before the fork (MiB)")
    ax.set_ylabel("parent paused (p50)")
    ax.legend(loc="center left")
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
    ax.xaxis.set_major_formatter(FuncFormatter(lambda v, _: fmt_ns(v)))
    ax.set_xlim(0, max(r[0] for r in rows) * 1.15)
    ax.grid(axis="y", visible=False)
    ax.set_xlabel("time per 4 KiB page (median of the children)")
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


IDLE_SETTINGS = [("", "defaults"), ("ksm", "KSM"), ("balloon", "balloon"), ("thp", "THP always")]


def chart_idle(res: Results, out: Path):
    # What each VM costs once idle: the last sample of each watch.
    cost: dict[tuple[str, str], float] = {}
    count = 0
    for params, runs in res.points("idle").items():
        p = dict(params)
        samples = runs[0].get("extra", {}).get("samples", [])
        if samples:
            cost[(p.get("features", ""), p.get("strategy", "share"))] = samples[-1].get("pss_total", 0) / p["vms"]
            count = p["vms"]
    if not cost:
        return
    settings = [(f, label) for f, label in IDLE_SETTINGS if any(k[0] == f for k in cost)]
    strategies = [s for s in RUNGS if any(k[1] == s for k in cost)]
    h = 0.8 / len(strategies)
    fig, ax = plt.subplots(figsize=(7.5, 0.9 + 0.8 * len(settings)))
    labeled: set[str] = set()
    for row, (f, _) in enumerate(reversed(settings)):
        for j, st in enumerate(strategies):
            v = cost.get((f, st))
            if v is None:
                continue
            y = row + 0.4 - h * (j + 0.5)
            ax.barh(y, v, h * 0.9, color=color(st), label=None if st in labeled else st)
            labeled.add(st)
            ax.text(v, y, f" {v:.0f} MiB", va="center", color=INK2, fontsize=8)
    ax.set_yticks(range(len(settings)), [label for _, label in reversed(settings)])
    ax.set_xlabel("PSS per VM after 2 minutes idle (MiB)")
    ax.set_xlim(right=max(cost.values()) * 1.15)
    ax.grid(axis="y", visible=False)
    handles, labels = ax.get_legend_handles_labels()
    order = sorted(range(len(labels)), key=lambda k: strategies.index(labels[k]))
    legend_below(fig, ax, len(order), handles=[handles[k] for k in order], labels=[labels[k] for k in order])
    finish(fig, ax, out / "idle-memory.png", "What an idle VM costs the host",
           f"Mean PSS of {count} idle 256 MiB VMs, by how they were made and the host setting")


# The comparison between hosts: the same operations, the p50 of each. Hosts
# are the entities here, so they get colors of their own: the first (nested,
# when it's given first) recessive.
HOST_COLORS = [MUTED, SLOTS[0], SLOTS[2]]
COMPARE = [
    ("create, cold boot", "create-cold", {}, "total"),
    ("create from a template", "create-template", {"strategy": None}, "total"),
    ("exec `true`", "exec", {}, "client"),
    ("snapshot 1 GiB: pause", "snapshot", {"memory_mib": 1024}, "paused"),
    ("rollback", "rollback", {}, "rollback"),
    ("fork 10 children", "fork", {"children": 10, "strategy": "share"}, "total"),
    ("fork 30 children", "fork", {"children": 30, "strategy": "share"}, "total"),
    ("fork 10, page server", "fork", {"children": 10, "strategy": "uffd-prefetch"}, "total"),
]


def compare_value(res: Results, scenario: str, want: dict, key: str) -> float | None:
    for params, runs in res.points(scenario).items():
        p = dict(params)
        if p.get("features") or "touch_mib" in p or "dirty_mib" in p:
            continue
        if all(p.get(k) == v for k, v in want.items()):
            v = timing(runs, key)
            if v:
                return statistics.median(v)
    return None


def chart_compare(sets: list[Results], out: Path):
    rows = []
    for label, scenario, want, key in COMPARE:
        vals = [compare_value(res, scenario, want, key) for res in sets]
        if all(v is not None for v in vals):
            rows.append((label, vals))
    # A child's first write to a shared page, in ms like the rest.
    touch = []
    for res in sets:
        t = [r["extra"]["touch_ns_per_page_p50"] for params, runs in res.points("fork").items()
             for r in runs if dict(params).get("strategy") == "share" and not dict(params).get("features")
             and "touch_mib" in dict(params) and "read" not in dict(params) and "touch_ns_per_page_p50" in r.get("extra", {})]
        touch.append(statistics.median(t) / 1e6 if t else None)
    if all(v is not None for v in touch):
        rows.append(("a child's first write to a page", touch))
    if not rows:
        return
    h = 0.8 / len(sets)
    fig, ax = plt.subplots(figsize=(7.5, 0.9 + 0.7 * len(rows)))
    for i, (label, vals) in enumerate(reversed(rows)):
        for j, v in enumerate(vals):
            y = i + 0.4 - h * (j + 0.5)
            ax.barh(y, v, h * 0.9, color=HOST_COLORS[j % len(HOST_COLORS)], label=sets[j].label if i == 0 else None)
            ax.text(v, y, f" {fmt_ms(v) if v >= 0.1 else f'{v * 1000:.3g} µs'}", va="center", color=INK2, fontsize=8)
    ax.set_yticks(range(len(rows)), [label for label, _ in reversed(rows)])
    ax.set_xscale("log")
    ax.xaxis.set_major_formatter(FuncFormatter(lambda v, _: fmt_ms(v) if v >= 0.1 else f"{v * 1000:g} µs"))
    ax.set_xlabel("p50, log scale")
    ax.set_xlim(right=max(v for _, vals in rows for v in vals) * 4)
    ax.grid(axis="y", visible=False)
    legend_below(fig, ax, len(sets), fontsize=8)
    finish(fig, ax, out / "compare.png", "The same operations, on each host",
           "Median of each; nested virtualization against bare metal")


def charts(sets: list[Results], out: Path):
    out.mkdir(parents=True, exist_ok=True)
    if len(sets) > 1:
        chart_compare(sets, out)
        chart_fork(sets, out, "total", "fork-latency.png", "Forking: total time by number of children",
                   "Snapshot of the parent, then every child restored (p50); cold boots for comparison",
                   "fork, total (p50)", log=True, rungs=["cold", "share", "uffd-prefetch"])
        chart_snapshot(sets, out)
        return
    chart_create_cdf(sets, out)
    chart_snapshot(sets, out)
    chart_fork(sets, out, "total", "fork-latency.png", "Forking: total time by number of children",
               "Snapshot of the parent, then every child restored (p50)", "fork, total (p50)", log=True)
    chart_fork(sets, out, "paused", "fork-paused.png", "Forking: how long the parent is paused",
               "Only the snapshot pauses the parent (p50)", "parent paused (p50)", legend="lower right")
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
