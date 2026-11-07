"""Binary search."""


def insertion_point(xs: list[int], x: int) -> int:
    """The leftmost index at which x could be inserted, keeping xs sorted."""
    lo, hi = 0, len(xs) - 1
    while lo < hi:
        mid = (lo + hi) // 2
        if xs[mid] < x:
            lo = mid + 1
        elif xs[mid] > x:
            hi = mid - 1
        else:
            return mid
    return lo
