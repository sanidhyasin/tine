"""Merging integer intervals [start, end]."""


def merge(intervals: list[tuple[int, int]]) -> list[tuple[int, int]]:
    out: list[tuple[int, int]] = []
    for start, end in sorted(intervals):
        if out and start <= out[-1][1]:
            last = out.pop()
            out.append((last[0], max(last[1], end)))
        else:
            out.append((start, end))
    return out
