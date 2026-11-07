from matrix import rotate


def test_rectangular():
    assert rotate([[1, 2, 3], [4, 5, 6]]) == [[4, 1], [5, 2], [6, 3]]


def test_four_times_is_identity():
    m = [[1, 2, 3], [4, 5, 6], [7, 8, 9], [10, 11, 12]]
    r = m
    for _ in range(4):
        r = rotate(r)
    assert r == m


def test_empty_and_single():
    assert rotate([]) == []
    assert rotate([[7]]) == [[7]]
