from intervals import merge


def test_unsorted_input():
    assert merge([(5, 7), (1, 3), (2, 4)]) == [(1, 4), (5, 7)]
