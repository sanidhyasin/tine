from datetime import date

from businessdays import business_days


def test_starting_on_saturday():
    # Sat 2026-11-07 to Wed 2026-11-11: Mon and Tue
    assert business_days(date(2026, 11, 7), date(2026, 11, 11)) == 2
