# Best-of-N bug fixing, with forks

The demo of what fast forking is for. An agent fixing a bug is a gamble: one attempt might fix it, or might only make the visible test pass. Five attempts at once are better odds, if setting up five copies of the repository is cheap. With tine it is: set the repository up once, fork the running sandbox five times, and every attempt starts from the same set-up machine in a fraction of a second.

For each task in `tasks/`, `run.py`:

1. creates a sandbox from the `python` image, uploads the repository and sets it up (git, a first pytest run);
2. forks it N times (5 by default);
3. runs N agent attempts at once, one per fork: Claude, with tools to run commands and read and write files;
4. runs the hidden tests, which the agent never sees, in each fork;
5. keeps the passing fix that changes the fewest lines, and destroys the rest;
6. prints each task's result, best-of-1 against best-of-N, the timings, the tokens and the cost.

## The tasks

Ten small Python repositories, each with a bug, a visible failing test, hidden tests that check the fix is general, and a reference fix. `uv run --with pytest python check_tasks.py` checks each one: its visible tests fail as it is, and pass, with the hidden ones, once fixed.

| Task | The bug |
|---|---|
| `bisect` | Binary search returns the wrong insertion point with duplicates and at the ends |
| `businessdays` | Counting weekdays goes wrong when the range starts on a weekend |
| `csvline` | A CSV line splits inside quoted fields |
| `intervals` | Merging intervals ignores their order, and touching ones |
| `lru` | An LRU cache evicts a key that was just read |
| `matrix` | Rotating a matrix transposes it, and fails on non-square ones |
| `roman` | Roman numerals without subtractive pairs (IV, IX, XL...) |
| `semver` | Versions compared as strings, and pre-releases wrongly |
| `tokenbucket` | A rate limiter refills without bound |
| `wordfreq` | Word counts that care about case and punctuation |

## Running it

It needs tined with the `python` image (`make python-image` builds it on the dev server), and [uv](https://docs.astral.sh/uv/). From a checkout, against the dev server:

```bash
make demo                                    # all tasks, 5 attempts each, with Claude
make demo DEMO_ARGS="--scripted"             # no model: checks the pipeline
make demo DEMO_ARGS="-n 3 --tasks roman,lru --compare-setup"
```

or directly, against tined on any machine:

```bash
cd examples/best-of-n
uv run run.py --host ssh://user@server -n 5 --budget 5
```

Claude needs an Anthropic API key (`ANTHROPIC_API_KEY`, or `ant auth login`). The demo runs where you start it, so the key never leaves your machine; only the sandboxes run on the server. It uses the latest Sonnet (`--model` picks another), and stops making requests once `--budget` dollars (10 by default) are spent.

`--scripted` replaces the model with a scripted stand-in that writes the reference fix in two attempts out of three: it checks forking, the attempts, the hidden tests and the report, without a model. Its success rates mean nothing.

`--compare-setup` also times the other way of getting N sandboxes: creating N and setting each up.

## Results

On the nested dev server, five attempts per task: with Claude Sonnet 5.5, one attempt fixed 88% of the bugs and the best of five 9 of the 10 tasks, for $0.43; with Claude Haiku 5.5, 90% and all 10, for $0.04. [`results/`](results) has every attempt, with its diff and, when it failed, the tests' output, and [results.md](../../docs/results.md#the-demo-best-of-n) what they show.
