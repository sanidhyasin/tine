from csvline import parse_line


def test_quoted_comma():
    assert parse_line('1,"Smith, John",42') == ["1", "Smith, John", "42"]
