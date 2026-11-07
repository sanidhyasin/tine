from wordfreq import top_words


def test_ties_alphabetical():
    assert top_words("b a c b a c", 3) == [("a", 2), ("b", 2), ("c", 2)]


def test_punctuation_everywhere():
    assert top_words("Hello, world! Hello? (world) hello...", 2) == [("hello", 3), ("world", 2)]


def test_apostrophes_kept():
    assert top_words("don't don't do", 1) == [("don't", 2)]


def test_n_larger_than_words():
    assert top_words("one", 5) == [("one", 1)]


def test_empty():
    assert top_words("", 3) == []
