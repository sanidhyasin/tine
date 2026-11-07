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
        self.tokens = min(self.capacity, self.tokens + (now - self.last) * self.rate)
        self.last = now
        if self.tokens >= 1:
            self.tokens -= 1
            return True
        return False
