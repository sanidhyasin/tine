from tokenbucket import TokenBucket


class Clock:
    def __init__(self):
        self.t = 0.0

    def __call__(self):
        return self.t


def test_burst_capped_after_waiting():
    c = Clock()
    b = TokenBucket(rate=1, capacity=2, clock=c)
    c.t = 100
    assert [b.allow() for _ in range(4)] == [True, True, False, False]
