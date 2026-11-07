"""A least-recently-used cache."""

from collections import OrderedDict


class LRUCache:
    def __init__(self, capacity: int) -> None:
        if capacity < 1:
            raise ValueError("capacity must be at least 1")
        self.capacity = capacity
        self._items: OrderedDict = OrderedDict()

    def get(self, key, default=None):
        return self._items.get(key, default)

    def put(self, key, value) -> None:
        self._items[key] = value
        if len(self._items) > self.capacity:
            self._items.popitem(last=False)

    def __len__(self) -> int:
        return len(self._items)

    def __contains__(self, key) -> bool:
        return key in self._items
