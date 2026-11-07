import pytest

from roman import from_roman, to_roman


@pytest.mark.parametrize("n,s", [(1, "I"), (3, "III"), (4, "IV"), (14, "XIV"), (40, "XL"), (90, "XC"),
                                 (400, "CD"), (944, "CMXLIV"), (1994, "MCMXCIV"), (3999, "MMMCMXCIX")])
def test_known(n, s):
    assert to_roman(n) == s
    assert from_roman(s) == n


def test_round_trip():
    for n in range(1, 4000):
        assert from_roman(to_roman(n)) == n


def test_out_of_range():
    with pytest.raises(ValueError):
        to_roman(0)
