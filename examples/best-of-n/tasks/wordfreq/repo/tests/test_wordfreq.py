from wordfreq import top_words


def test_case_and_punctuation():
    assert top_words("The the THE. cat", 1) == [("the", 3)]
