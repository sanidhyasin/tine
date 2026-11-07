"""Matrix helpers."""


def rotate(m: list[list[int]]) -> list[list[int]]:
    """m rotated 90 degrees clockwise."""
    n = len(m)
    return [[m[j][i] for j in range(n)] for i in range(n)]
