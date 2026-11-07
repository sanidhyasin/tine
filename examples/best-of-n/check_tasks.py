"""Check every task: its visible tests fail as it is, and with the reference
fix, its visible and hidden tests all pass.

    uv run --with pytest python check_tasks.py
"""

from __future__ import annotations

import json
import shutil
import subprocess
import sys
import tempfile
from pathlib import Path

TASKS = Path(__file__).parent / "tasks"


def pytest(repo: Path) -> bool:
    r = subprocess.run(
        [sys.executable, "-m", "pytest", "-q", "-p", "no:cacheprovider", "tests"],
        cwd=repo,
        capture_output=True,
        timeout=120,
    )
    return r.returncode == 0


def main() -> int:
    bad = 0
    for task in sorted(p for p in TASKS.iterdir() if p.is_dir()):
        meta = json.loads((task / "task.json").read_text())
        with tempfile.TemporaryDirectory() as tmp:
            repo = Path(tmp) / "repo"
            shutil.copytree(task / "repo", repo)
            fails_before = not pytest(repo)
            shutil.copy(task / "solution" / meta["module"], repo / meta["module"])
            shutil.copy(task / "hidden" / "test_hidden.py", repo / "tests" / "test_hidden.py")
            passes_after = pytest(repo)
        ok = fails_before and passes_after
        bad += not ok
        print(
            f"{'ok ' if ok else 'BAD'}  {meta['name']:14} visible tests fail as is: {fails_before}; "
            f"all pass with the fix: {passes_after}"
        )
    return 1 if bad else 0


if __name__ == "__main__":
    sys.exit(main())
