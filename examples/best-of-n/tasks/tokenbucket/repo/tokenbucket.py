"""A token bucket rate limiter, with an injectable clock for testing."""

import time


class TokenBucket:
    def __init__(self, rate: float, capacity: int, clock=time.monotonic) -> None:
        self.rate = rate  # tokens per second
        self.capacity = capacity
        self.clock = clock
        self.tokens = float(capacity)
        self.last = clock()

    def allow(self) -> bool:
        now = self.clock()
        self.tokens += (now - self.last) * self.rate
        if self.tokens >= 1:
            self.tokens -= 1
            return True
        return False
