# tine: project brief

*Fork a running microVM into many copies in milliseconds.*

Last updated: 2026-11-07

## What it is

tine is a sandbox system built on Firecracker microVMs. It takes a **running** sandbox, including its files and everything in memory (running processes, loaded data), and forks it into N independent copies in milliseconds. It can also roll a sandbox back to a save point.

The idea is a video-game save point, or `git branch`, for a whole running computer.

## Why build it

- **Goal:** fast, cheap copies of a running sandbox, for agents, RL environments and serverless workloads.
- **The gap:** live VM forking already exists (E2B, Morph, CodeSandbox, Daytona, and open-source projects like sandstorm and zeroboot). Their published numbers are self-reported and often contradict each other, and almost nobody reports how much slower a fork runs afterwards. tine's main output is an honest, reproducible breakdown of where the milliseconds go.
- **The writeup is the main deliverable.** The code backs it up with one command that reruns the benchmarks on any machine with KVM.

## Use cases

1. **Try many approaches at once:** an agent forks into N copies, each tries something different, and the best one is kept.
2. **Undo for agents:** checkpoint before a risky step and roll back after a bad one.
3. **RL and tree search:** try many actions from exactly the same environment state.
4. **Instant startup:** set up a template once, then fork new sandboxes from it. For example, one fork per lab or student instead of provisioning each from scratch.

## How it fits together

```
Your Mac ──(ssh)──▶ Linux server with KVM
                    ┌───────────────────────────────────────────┐
                    │  MANAGER (Go)  API: create/exec/fork/...  │
                    │   • starts Firecracker processes          │
                    │   • one network namespace per VM          │
                    │   • copy-on-write disk copies             │
                    │   • pause / snapshot / restore            │
                    │                                           │
                    │  Firecracker   Firecracker   Firecracker  │  ← downloaded, not written
                    │   ┌──────┐      ┌──────┐      ┌──────┐    │
                    │   │ VM 1 │      │ VM 2 │      │ VM 3 │    │
                    │   │agent │      │agent │      │agent │    │  ← small Go program in each VM
                    │   └──────┘      └──────┘      └──────┘    │
                    └───────────────────────────────────────────┘
        clients:  tine CLI (you, benchmarks)  ·  Python SDK (agent demo)
```

## Interface

The CLI is the main way to use tine:

```
tine create --image python
tine exec <id> -- <command>
tine fork <id> --n 5
tine snapshot <id>
tine rollback <id>
tine ls --tree
tine destroy <id>
```

Every command prints how long it took.

The **Python SDK** is a thin client over the manager's API (`sb.fork(5)`) and is used by the agent demo. There is no dashboard.

## Stack

| Piece | Choice |
|---|---|
| Host | Linux with KVM. For development, AWS C8i/M8i with nested virtualization. For final benchmarks, bare metal. |
| VMM | Firecracker, pinned to v1.16.2 / v1.17.0 or later |
| Manager, CLI, in-VM agent | Go |
| SDK, benchmarks, demo | Python |
| Disk copy-on-write | Reflinks (XFS/btrfs) to start; compare dm-thin later |
| Networking | One network namespace per VM, so every clone can keep the same tap device name; NAT for outbound traffic |
| Workflow | Build on the Mac; run and test on the Linux server over SSH |

## Build order

Each step works on its own before the next one starts.

| # | Step | Done when |
|---|---|---|
| 0 | Boot one VM by hand using Firecracker's getting-started guide. No code. | You have a shell inside a VM you started yourself |
| 1 | Go manager and CLI: `create`, `exec`, `destroy` | `tine exec` returns a command's output |
| 2 | Snapshot and rollback | You can break a VM and roll it back |
| 3 | Fork into N copies: shared memory, copy-on-write disks, separate networks, reseeded randomness | 10 forks run independently with different IP addresses |
| 4 | Measure, then make it faster (see the ladder below) | Every rung is measured |
| 5 | Python SDK, agent demo, writeup | Writeup published |

## The improvement ladder (the writeup's storyline)

| Rung | Approach | Expected tradeoff |
|---|---|---|
| 1 | Cold boot | Baseline |
| 2 | Naive fork: dump and copy all memory | About 1 s per GB, and it gets worse as RAM grows |
| 3 | Restore by mapping the snapshot file, so pages are shared until a copy writes to them | Many clones from one parent are nearly free, but forking a *running* VM is still slow |
| 4 | Save only the pages that changed (diff snapshots or chained layers) | Fork cost depends on how much memory changed, not total size |
| 5 | Load pages lazily with userfaultfd | Fast forks, but the cost moves to page faults after the fork |

## What we measure

Every rung gets the same measurements:

- **Fork latency, p50 and p99**, split into pause, memory capture, disk copy, restore and network setup.
- **Memory per fork** and **sandboxes per host**.
- **Slowdown right after a fork**, from pages being loaded on first use.
- **What breaks at the limit:** host memory pressure as forks drift apart, network namespace / tap / file descriptor limits, NVMe vs tmpfs, and huge pages or KSM interacting with copy-on-write.

## Out of scope

Dashboard, auth, multi-host scheduling, Kubernetes, MCP server and billing are all out. If there's time at the very end, a live fork-tree view for the demo video.

## Gotchas

- **Firecracker won't run on a Mac.** Always build and run tine on the Linux server.
- **Forks start out too identical:** same random number state, same MAC and IP address, same open TCP connections, and clocks that need fixing. Use Firecracker's VMGenID device and a separate network namespace per VM.
- **Capture memory and disk at the same paused moment**, or they won't match.
- **CVE-2026-122192:** restoring from a memory file while using a balloon or virtio-mem device lets the guest reach Firecracker's own memory. Use v1.16.2 / v1.17.0 or later.
- **Nested virtualization skews the numbers.** Label development numbers as such, and do final runs on bare metal.

## Reading list

You don't need to read all of this before starting. Read each item when you reach the step it's for. Everything in the main list is from 2026; the Firecracker docs are kept up to date on the main branch.

**Before you start (about an hour)**
- [DigitalOcean: Inside the MicroVM architecture behind Managed Agents](https://www.digitalocean.com/blog/microvm-architecture) (updated Oct 2026). The closest thing to a blueprint for tine: Firecracker, restore with userfaultfd, copy-on-write disk overlays, clones that all share one IP and MAC but are isolated by network namespaces, and a latency budget for each stage. Under 200 ms at p50 for their warm path.
- [Hugging Face: One sandbox per rollout, or how labs run RL for agents in 2026](https://huggingface.co/blog/sergiopaniego/rl-environments-2026) (Sep 2026). How AI labs use sandboxes for RL training, which is the "why" behind tine.

**Steps 0–1: booting VMs**
- [Firecracker getting started](https://github.com/firecracker-microvm/firecracker/blob/main/docs/getting-started.md)
- [Firecracker CHANGELOG](https://github.com/firecracker-microvm/firecracker/blob/main/CHANGELOG.md) and the [CVE-2026-122192 bulletin](https://aws.amazon.com/security/security-bulletins/2026-169-aws/), to pick a safe version
- [EC2 nested virtualization announcement](https://aws.amazon.com/about-aws/whats-new/2026/02/amazon-ec2-nested-virtualization-on-virtual) (Feb 2026)

**Steps 2–3: snapshots and forks**
- Firecracker docs: [snapshot support](https://github.com/firecracker-microvm/firecracker/blob/main/docs/snapshotting/snapshot-support.md), [random for clones](https://github.com/firecracker-microvm/firecracker/blob/main/docs/snapshotting/random-for-clones.md), [network for clones](https://github.com/firecracker-microvm/firecracker/blob/main/docs/snapshotting/network-for-clones.md)
- [E2B fork docs](https://docs.e2b.dev/sandbox/fork). A good reference for what tine's fork should promise: the original keeps running, one snapshot serves any number of forks, and each fork succeeds or fails on its own.
- [Fly.io: The Design & Implementation of Sprites](https://fly.io/blog/design-and-implementation/) (Jan 2026). Treats checkpoints as a routine, git-like operation, around 300 ms each.

**Steps 4–5: measuring and scale**
- [DeepSeek: DSec, a sandbox infrastructure for agentic training at scale](https://arxiv.org/abs/2609.22978) (Sep 2026). Production sandbox infrastructure for RL, on Firecracker and containers: over 380,000 concurrent sandboxes and up to 800 microVMs per node. A real-world reference point for tine's density numbers.
- *Optional:* [Aquifer: memory pooling for microVM snapshots](https://arxiv.org/abs/2606.24079) (Jun 2026). Research on splitting snapshot memory into hot and cold pages to speed up restore.

**Prior art:** E2B, [Morph Cloud](https://cloud.morph.so/docs/developers), Daytona, [deeplethe/sandstorm](https://github.com/deeplethe/forkd/wiki), [zeroboot](https://awesome.ecosyste.ms/projects/github.com%2Fzerobootdev%2Fzeroboot), [paperclipinc/sandbox](https://github.com/paperclipinc/sandbox)

**Older but still clear (optional):** CodeSandbox's [How we clone a running VM in 2 seconds](https://dev.to/codesandboxio/how-we-clone-a-running-vm-in-2-seconds-39m3) (2022) and [Cloning microVMs using userfaultfd](https://codesandbox.io/blog/cloning-microvms-using-userfaultfd); the paper [Restoring Uniqueness in MicroVM Snapshots](https://ar5iv.labs.arxiv.org/html/2102.12892) (2021)
