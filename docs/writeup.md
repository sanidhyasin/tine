# Forking a running computer in a few milliseconds

*The writeup for tine: what it does, how, what it measured, and what broke. Every number in it is from [results.md](results.md), and reproducible with one command.*

## The problem: agents need many copies of a running computer

An AI agent fixing a bug in a repository needs a computer: a shell to run the tests, a filesystem to change, a network to install from. Giving it one is easy. Giving it the *right* one is the hard part: a machine where the repository is already cloned, the dependencies installed, the database seeded and the test suite warmed up. Setting that up takes minutes. The agent's attempt might take thirty seconds.

And one attempt isn't enough. Agents are gamblers: the same agent, on the same bug, sometimes fixes it and sometimes doesn't. Five attempts at once, keeping the one that passes, solve more tasks than one. Reinforcement learning wants more still: thousands of rollouts, each from the same starting state, each free to wreck its machine.

So what agents and RL want isn't a computer. It's *many copies of one running computer*, taken at exactly the moment the setup finished, and they want them now. Containers can't copy a running process. Cloud VMs take tens of seconds to boot and can't be copied mid-run at all. That gap is what tine fills: it forks a running microVM into many running copies in a fraction of a second, the way `fork()` copies a process.

## How forking works

A VM is mostly memory. tine runs each sandbox as a [Firecracker](https://firecracker-microvm.github.io/) microVM: a real Linux kernel on minimal virtual hardware, isolated by KVM, a jail, its own user, cgroup and network namespace.

Forking a VM is three steps:

1. **Freeze it for a moment and save it.** Firecracker writes the VM's memory and device state to files, and tine copies its disk as a reflink, which is instant on XFS because the copy shares the original's blocks. A 256 MiB VM is paused for 64 ms, then carries on as if nothing happened.
2. **Start copies from the save.** Each child is a new Firecracker that *maps* the saved memory file instead of reading it. Nothing is copied: the child's pages are the file's pages, in the host's page cache.
3. **Copy on write.** When a child writes to a page, the kernel gives it a private copy of that page, and only that page. Thirty children that each change a little memory cost little more than one.

Then each child is made its own machine again: a new hostname and machine ID, fresh randomness for its kernel, the right time, its own network and disk. It wakes up mid-instruction, exactly where its parent was, with every process still running.

On the dev server, a 4-vCPU cloud VM running Firecracker through nested virtualization, forking a 256 MiB VM into 10 takes 398 ms, and into 30, 1.02 s. Booting 10 or 30 fresh VMs instead takes 7.5 s and 21 s. An idle child costs the host 14 MiB, against 109 MiB for a booted VM: nearly all its memory is its parent's, shared. <!-- METAL-INTRO -->

## The ladder

There's more than one way to give a forked VM its memory. tine can do five, so they could be measured side by side, on the same host, the same way:

1. **Boot fresh VMs.** The baseline: nothing shared, every machine sets itself up again.
2. **Copy the memory.** Save once, then give every child its own copy of the memory file.
3. **Share the memory.** Every child maps the one file, and copy-on-write does the rest. tine's default.
4. **Diff snapshots.** The parent tracks the pages it writes, so its next save writes only those, and the full file is built afterwards, while the parent runs.
5. **A page server.** The child starts with no memory at all. A small server, `tine-uffd`, copies each page in through userfaultfd the moment the guest touches it, and can copy in the pages a restore will need before it asks.

| Fork of a 256 MiB VM into | 1 | 10 | 30 | Each child's memory, of 30 |
|---|---|---|---|---|
| Boot fresh VMs | 1.90 s | 7.49 s | 21.3 s | 109 MiB |
| Copy | 218 ms | 803 ms | 11.0 s | 95 MiB |
| **Share** | **138 ms** | **398 ms** | **1.02 s** | **14 MiB** |
| Diff snapshots | 153 ms | 458 ms | 1.17 s | 14 MiB |
| Page server | 176 ms | 578 ms | 1.61 s | 20 MiB |
| Page server, prefetching | 170 ms | 462 ms | 1.11 s | 20 MiB |

Sharing wins, and the reasons are instructive:

- **Copying** pays for bytes nobody may ever read. Ten children copy 2.5 GiB, which the page cache absorbs. Thirty copy 7.5 GiB on a 16 GiB host, the kernel starts writing copies to the disk to make room, and the fork takes 11 s.
- **A page server** pays for every page one fault at a time. A guest waking up touches thousands of pages, and each is a round trip: fault, wake the server, copy, wake the guest. Sharing maps pages the page cache already has, with no server in the loop. Recording what the first restore touched and copying it all in up front (prefetching) wins most of that back, and makes the page server the fastest way to create a single VM from a template: 41 ms, against 57 ms for sharing.
- **Diff snapshots** don't make a fork faster, because a fresh parent's first snapshot is a full one. What they change is every snapshot after: a VM that changed little is paused for 4 to 6 ms, from 256 MiB to 4 GiB, instead of 48 ms to 601 ms.

The parent's pause doesn't depend on the strategy or on the number of children. It's the snapshot, before any child exists.

**Where the time goes.** A create from a template, in the default strategy, is 57 ms: 5 ms of tine's own setup (an ID, a network namespace, a jail), 12 ms for Firecracker to load and resume the snapshot, 20 ms for the guest to wake up and its agent to answer, and 18 ms for the agent to give it the right time, randomness and identity. Most of it is the guest touching memory as it wakes. That's why prefetching helps, and why 2 MiB huge pages help more: one fault brings in 512 times as much, and a child's first write to its parent's memory costs 1.1 µs per 4 KiB page instead of 62 µs.

## What broke

The interesting results are the failures. Each of these cost time to find:

- **A snapshot that froze a VM for 22 seconds.** Saving a 4 GiB VM writes 4 GiB of new dirty pages at once, and the kernel throttles a process that dirties pages faster than the disk takes them. The VM stays paused while it waits. Raising `vm.dirty_ratio` from 20 to 60 brought it to 601 ms at the median, and 4.2 s at the p99, and diff snapshots avoid it for VMs that change little.
- **Back-to-back snapshots that paused for a second.** Right after a snapshot's memory reached the disk, the next snapshot of the same VM paused it for up to 1.3 s instead of 60 ms. The kernel was still throttling the VM's cgroup, which had just written gigabytes. tined now waits until 2 s after the last flush.
- **Children that each finished their parent's boot.** A parent forked the moment its agent answered was still booting, and each child finished the boot on its own: 30 children took 5 s instead of 1. Forked when systemd said the boot was done, each child finished systemd's last units instead: 850 ms for 10 instead of 450. tine now waits for systemd, then 2 s more. The same fix took creating a VM from a template from 111 ms to 57.
- **Running the host out of memory.** Admission control counts what VMs use now, and children share almost everything, so it let 24 children of a 2 GiB VM in on a 16 GiB host. Then each changed more of its memory, round after round. At 640 MiB each, the kernel ran out, and its OOM killer killed whole VMs, 2 to 5 of the 24. tined noticed and marked them failed, and the rest kept running.
- **Nested virtualization's limit.** Past about 60 guest vCPUs, EC2's nested virtualization refuses to enter more guests, so the dev server tops out at 31 children of one VM, with 14 GiB of memory still free.
- **An experiment that didn't evict.** To see what happens when a fork's memory is pushed out of the page cache, the benchmark dropped the cache under children reading their parent's memory. The reads stayed fast. The kernel can't drop pages a process has mapped, and each child maps about 100 MiB of its parent's memory as it wakes. Real eviction takes real memory pressure.

<!-- METAL-WRITEUP -->

## The demo: best-of-N with forks

The demo is the use case in miniature. Ten small Python repositories each have a bug, a visible failing test and hidden tests the agent never sees. For each:

1. Create one sandbox from a Python image, upload the repository, and set it up: `git init`, a first `pytest` run.
2. Fork it five times.
3. Run five agent attempts at once, one per fork. The agent is Claude, with tools to run commands and read and write files.
4. Run the hidden tests in every fork, and keep the passing fix that changes the fewest lines.

It's about 500 lines of Python on tine's SDK, half of them the agent. The part that's tine:

```python
async with AsyncClient(host="ssh://tine-dev") as tine:
    async with await AsyncSandbox.create("python", client=tine) as sb:
        await sb.upload(task_dir / "repo", "/root/repo")
        await sb.exec("cd /root/repo && git init -q && git add -A && git commit -qm start", check=True)
        forks = await sb.fork(5)
        attempts = await asyncio.gather(*(attempt(llm, f, issue, i, model=model, budget=budget) for i, f in enumerate(forks)))
```

Forking five set-up sandboxes took about half a second, where setting up each of these tiny repositories took 1.0 to 4.7 s. For one of them, setting it up once and forking it took 1.8 s, and setting up five sandboxes separately 4.9 s. Real repositories, with dependencies to install, take minutes to set up; the fork stays under a second.

<!-- DEMO-RESULTS: best-of-1 against best-of-5 with Claude, from `make demo` -->

## What's next

- **Live fork.** Today the parent pauses while its memory is saved. With userfaultfd's write protection, it could keep running while its memory is copied out underneath it, pausing only for the pages it touches.
- **More than one host.** Forks share memory and disk with their parent, which only works on one machine. Moving snapshots between hosts, and children with them, is the next frontier.
- **Tree search.** The demo forks once, at the start. An agent that forks at every step, keeping the branches that look promising, is the natural next experiment.

tine is on GitHub at [github.com/sanidhyasin/tine](https://github.com/sanidhyasin/tine), with every number in this post reproducible with one command.
