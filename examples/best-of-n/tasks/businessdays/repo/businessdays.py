"""Counting business days."""

from datetime import date, timedelta


def business_days(start: date, end: date) -> int:
    """Weekdays in [start, end)."""
    days = (end - start).days
    weeks, extra = divmod(days, 7)
    return weeks * 5 + min(extra, 5)


def add_business_days(start: date, n: int) -> date:
    d = start
    while n > 0:
        d += timedelta(days=1)
        if d.weekday() < 5:
            n -= 1
    return d
