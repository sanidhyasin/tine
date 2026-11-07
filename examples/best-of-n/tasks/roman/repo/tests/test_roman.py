from roman import from_roman, to_roman


def test_four_and_nine():
    assert to_roman(4) == "IV"
    assert to_roman(9) == "IX"
