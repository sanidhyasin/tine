"""Parsing one line of CSV."""


def parse_line(line: str) -> list[str]:
    return [field.strip('"') for field in line.split(",")]
