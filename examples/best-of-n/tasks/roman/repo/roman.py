"""Roman numerals."""

_VALUES = [(1000, "M"), (500, "D"), (100, "C"), (50, "L"), (10, "X"), (5, "V"), (1, "I")]


def to_roman(n: int) -> str:
    if not 0 < n < 4000:
        raise ValueError(f"{n} can't be written in Roman numerals")
    out = []
    for value, letters in _VALUES:
        while n >= value:
            out.append(letters)
            n -= value
    return "".join(out)


def from_roman(s: str) -> int:
    values = {letters: value for value, letters in _VALUES}
    total = 0
    for ch in s:
        total += values[ch]
    return total
