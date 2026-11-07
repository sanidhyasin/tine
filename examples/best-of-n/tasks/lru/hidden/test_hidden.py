import pytest

from lru import LRUCache


def test_put_existing_refreshes():
    c = LRUCache(2)
    c.put("a", 1)
    c.put("b", 2)
    c.put("a", 10)
    c.put("c", 3)
    assert c.get("a") == 10 and "b" not in c


def test_missing_get_changes_nothing():
    c = LRUCache(2)
    c.put("a", 1)
    c.put("b", 2)
    assert c.get("zzz", "none") == "none"
    c.put("c", 3)
    assert "a" not in c and len(c) == 2


def test_capacity_one():
    c = LRUCache(1)
    c.put(1, 1)
    c.put(2, 2)
    assert 1 not in c and c.get(2) == 2


def test_bad_capacity():
    with pytest.raises(ValueError):
        LRUCache(0)
