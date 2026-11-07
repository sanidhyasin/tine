"""Word frequencies."""

import re
from collections import Counter


def top_words(text: str, n: int) -> list[tuple[str, int]]:
    """The n most common words, most common first."""
    counts = Counter(re.findall(r"[a-z0-9']+", text.lower()))
    return sorted(counts.items(), key=lambda kv: (-kv[1], kv[0]))[:n]
