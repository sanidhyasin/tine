import bisect
import random

from search import insertion_point


def test_edges():
    assert insertion_point([], 5) == 0
    assert insertion_point([1, 2, 3], 0) == 0
    assert insertion_point([1, 2, 3], 4) == 3
    assert insertion_point([1, 3], 2) == 1


def test_against_bisect():
    rng = random.Random(7)
    for _ in range(2000):
        xs = sorted(rng.randint(0, 20) for _ in range(rng.randint(0, 15)))
        x = rng.randint(-2, 22)
        assert insertion_point(xs, x) == bisect.bisect_left(xs, x), (xs, x)
