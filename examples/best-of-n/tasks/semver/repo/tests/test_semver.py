from semver import compare


def test_numeric_parts():
    assert compare("1.10.0", "1.9.0") == 1
