from search import insertion_point


def test_duplicates_leftmost():
    assert insertion_point([1, 2, 2, 2, 2, 3], 2) == 1
