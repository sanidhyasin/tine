"""Comparing semantic versions."""


def compare(a: str, b: str) -> int:
    """-1 if a < b, 0 if equal, 1 if a > b."""
    return (a > b) - (a < b)
