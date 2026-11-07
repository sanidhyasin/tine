"""Matrix helpers."""


def rotate(m: list[list[int]]) -> list[list[int]]:
    """m rotated 90 degrees clockwise."""
    return [list(row) for row in zip(*m[::-1])]
