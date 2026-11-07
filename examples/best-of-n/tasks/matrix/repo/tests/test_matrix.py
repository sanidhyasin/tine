from matrix import rotate


def test_square():
    assert rotate([[1, 2], [3, 4]]) == [[3, 1], [4, 2]]
