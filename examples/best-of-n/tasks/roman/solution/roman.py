"""Roman numerals."""

_VALUES = [(1000, "M"), (900, "CM"), (500, "D"), (400, "CD"), (100, "C"), (90, "XC"), (50, "L"), (40, "XL"),
           (10, "X"), (9, "IX"), (5, "V"), (4, "IV"), (1, "I")]


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
    single = {"M": 1000, "D": 500, "C": 100, "L": 50, "X": 10, "V": 5, "I": 1}
    total = 0
    for i, ch in enumerate(s):
        v = single[ch]
        if i + 1 < len(s) and single[s[i + 1]] > v:
            total -= v
        else:
            total += v
    return total
