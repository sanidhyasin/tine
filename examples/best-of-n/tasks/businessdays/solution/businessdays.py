"""Counting business days."""

from datetime import date, timedelta


def business_days(start: date, end: date) -> int:
    """Weekdays in [start, end)."""
    if end <= start:
        return 0
    days = (end - start).days
    weeks, extra = divmod(days, 7)
    count = weeks * 5
    for i in range(extra):
        if (start.weekday() + i) % 7 < 5:
            count += 1
    return count


def add_business_days(start: date, n: int) -> date:
    d = start
    while n > 0:
        d += timedelta(days=1)
        if d.weekday() < 5:
            n -= 1
    return d
