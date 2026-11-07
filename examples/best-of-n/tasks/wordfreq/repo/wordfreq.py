"""Word frequencies."""

from collections import Counter


def top_words(text: str, n: int) -> list[tuple[str, int]]:
    """The n most common words, most common first."""
    counts = Counter(text.split())
    return counts.most_common(n)
