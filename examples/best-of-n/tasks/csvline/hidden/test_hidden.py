from csvline import parse_line


def test_plain():
    assert parse_line("a,b,c") == ["a", "b", "c"]


def test_empty_fields():
    assert parse_line(",,") == ["", "", ""]
    assert parse_line("") == [""]


def test_doubled_quotes():
    assert parse_line('"say ""hi""",x') == ['say "hi"', "x"]


def test_quoted_empty():
    assert parse_line('"",a') == ["", "a"]


def test_spaces_kept():
    assert parse_line(" a , b ") == [" a ", " b "]
