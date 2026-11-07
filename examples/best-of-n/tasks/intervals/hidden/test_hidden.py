from intervals import merge


def test_empty():
    assert merge([]) == []


def test_touching_merge():
    assert merge([(1, 2), (2, 3)]) == [(1, 3)]


def test_contained():
    assert merge([(1, 10), (2, 3), (4, 5)]) == [(1, 10)]


def test_disjoint_sorted_out():
    assert merge([(10, 12), (1, 2), (5, 6)]) == [(1, 2), (5, 6), (10, 12)]


def test_chain():
    assert merge([(3, 5), (1, 3), (5, 8), (9, 9)]) == [(1, 8), (9, 9)]
