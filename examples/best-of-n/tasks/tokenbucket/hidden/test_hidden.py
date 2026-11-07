from tokenbucket import TokenBucket


class Clock:
    def __init__(self):
        self.t = 0.0

    def __call__(self):
        return self.t


def test_rate():
    c = Clock()
    b = TokenBucket(rate=2, capacity=1, clock=c)
    assert b.allow() and not b.allow()
    c.t = 0.25
    assert not b.allow()
    c.t = 0.5
    assert b.allow() and not b.allow()


def test_no_double_counting():
    c = Clock()
    b = TokenBucket(rate=1, capacity=5, clock=c)
    for _ in range(5):
        assert b.allow()
    c.t = 1.0
    assert b.allow()
    assert not b.allow()
    assert not b.allow()
