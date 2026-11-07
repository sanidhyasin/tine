"""Comparing semantic versions."""


def _key(v: str):
    v = v.split("+", 1)[0]
    core, _, pre = v.partition("-")
    nums = tuple(int(x) for x in core.split("."))
    if not pre:
        return (nums, 1, ())
    parts = tuple((0, int(p), "") if p.isdigit() else (1, 0, p) for p in pre.split("."))
    return (nums, 0, parts)


def compare(a: str, b: str) -> int:
    """-1 if a < b, 0 if equal, 1 if a > b."""
    ka, kb = _key(a), _key(b)
    return (ka > kb) - (ka < kb)
