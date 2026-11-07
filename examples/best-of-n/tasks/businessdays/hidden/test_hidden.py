from datetime import date, timedelta

from businessdays import add_business_days, business_days


def brute(start, end):
    return sum(1 for i in range((end - start).days) if (start + timedelta(days=i)).weekday() < 5)


def test_against_counting():
    base = date(2026, 1, 1)
    for s in range(14):
        for length in range(0, 40):
            start = base + timedelta(days=s)
            end = start + timedelta(days=length)
            assert business_days(start, end) == brute(start, end), (start, end)


def test_backwards_is_zero():
    assert business_days(date(2026, 11, 11), date(2026, 11, 7)) == 0


def test_add():
    assert add_business_days(date(2026, 11, 6), 1) == date(2026, 11, 9)
