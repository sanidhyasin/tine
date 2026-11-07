#!/usr/bin/env bash
# Run all of step 4's benchmarks on this host, into one results file. Run it
# on the Linux host, with tined, tine-bench, tine-uffd and the bench image
# installed (`make deploy bench-image`); `make bench-all` runs it from a Mac
# and charts the results.
#
#   bench/run.sh [OUT.jsonl]          every part, about 3 hours on 4 CPUs
#   PARTS="base pages" bench/run.sh    only some parts
#   QUICK=1 bench/run.sh               fewer runs of each, to try it out
#
# Each part runs tine-bench against tined as it's installed, or restarted with
# a feature turned on through a systemd drop-in. Host settings a part changes
# (transparent huge pages, KSM, the huge page pool) are put back when it ends,
# and tined is restarted as it was, even if the run is stopped.
#
# Parts:
#   base     the suite: creates, execs, rollbacks, snapshots, and forks of each
#            strategy that shares nothing or the page cache (share, copy, cold)
#   pages    forks through a page server: uffd, uffd-prefetch
#   diff     tined --diff-snapshots: snapshots, and forks after memory changes
#   huge     page-server forks with 2 MiB huge pages
#   thp      transparent huge pages "always" on the host
#   ksm      KSM merging identical pages across idle VMs
#   balloon  booted VMs with a balloon that gives free pages back
#   breaks   what breaks: children reading memory evicted from the page cache,
#            and children changing memory until the host runs out (last, as
#            it ends with the kernel killing VMs)

set -euo pipefail

out=${1:-/tmp/tine-bench.jsonl}
parts=${PARTS:-base pages diff huge thp ksm balloon breaks}
service=/etc/systemd/system/tined.service
dropin=/etc/systemd/system/tined.service.d/bench.conf
thp=/sys/kernel/mm/transparent_hugepage/enabled
ksm=/sys/kernel/mm/ksm

runs=() idle=(-idle-for 2m)
if [[ -n ${QUICK:-} ]]; then
	runs=(-runs 10 -fork-runs 3 -snapshot-runs 3 -warmup 1)
	idle=(-idle-for 30s -idle-vms 8)
fi

log() { printf '\n==> %s\n' "$*" >&2; }

# tined_with FLAGS...: restart tined with these flags added (none: as installed).
tined_with() {
	if (($# == 0)); then
		sudo rm -f "$dropin"
	else
		local exec
		exec=$(sed -n 's/^ExecStart=//p' "$service")
		sudo mkdir -p "$(dirname "$dropin")"
		printf '[Service]\nExecStart=\nExecStart=%s %s\n' "$exec" "$*" | sudo tee "$dropin" >/dev/null
	fi
	sudo systemctl daemon-reload
	sudo systemctl restart tined
	for _ in $(seq 150); do
		if tine images >/dev/null 2>&1; then return; fi
		sleep 0.2
	done
	echo "tined didn't come back up" >&2
	exit 1
}

bench() { sudo tine-bench -out "$out" -append "${runs[@]}" "$@"; }

thp_was=$(sed 's/.*\[\(.*\)\].*/\1/' "$thp")
ksm_was=$(cat "$ksm/run")
pages_to_scan_was=$(cat "$ksm/pages_to_scan")
hugepages_was=$(cat /proc/sys/vm/nr_hugepages)
put_back() {
	echo "$thp_was" | sudo tee "$thp" >/dev/null
	# 2 unmerges everything KSM merged, then stops it.
	if [[ $(cat "$ksm/run") != "$ksm_was" ]]; then echo 2 | sudo tee "$ksm/run" >/dev/null; fi
	echo "$ksm_was" | sudo tee "$ksm/run" >/dev/null
	echo "$pages_to_scan_was" | sudo tee "$ksm/pages_to_scan" >/dev/null
	sudo sysctl -q vm.nr_hugepages="$hugepages_was"
	tined_with
}
trap put_back EXIT

: >"$out"
tined_with
for part in $parts; do
	case $part in
	base)
		log "base: the suite, with forks that share the page cache, copy it, or boot cold"
		bench -strategy share,copy,cold suite
		bench -strategy share,cold "${idle[@]}" idle
		;;
	pages)
		log "pages: forks through a page server"
		bench -strategy uffd,uffd-prefetch create-template fork fork-touch
		bench -strategy uffd "${idle[@]}" idle
		;;
	diff)
		log "diff: tined --diff-snapshots"
		tined_with --diff-snapshots
		bench -strategy share snapshot fork fork-dirty fork-touch
		tined_with
		;;
	huge)
		log "huge: page-server forks with 2 MiB huge pages"
		# Room for 10 children of 256 MiB, and their parent's fork.
		sudo sysctl -q vm.nr_hugepages=1536
		tined_with --page-server-huge-pages 2M
		bench -strategy uffd,uffd-prefetch -children 1,10 fork fork-touch
		tined_with
		sudo sysctl -q vm.nr_hugepages="$hugepages_was"
		;;
	thp)
		log "thp: transparent huge pages always on"
		echo always | sudo tee "$thp" >/dev/null
		bench -strategy share create-cold snapshot fork fork-touch
		bench -strategy share,cold "${idle[@]}" idle
		echo "$thp_was" | sudo tee "$thp" >/dev/null
		;;
	ksm)
		log "ksm: merging identical pages across idle VMs"
		# Fast enough to go through 20 VMs' memory within the watch.
		echo 4000 | sudo tee "$ksm/pages_to_scan" >/dev/null
		echo 1 | sudo tee "$ksm/run" >/dev/null
		tined_with --ksm
		bench -strategy cold,share,uffd "${idle[@]}" idle
		tined_with
		echo 2 | sudo tee "$ksm/run" >/dev/null
		echo "$ksm_was" | sudo tee "$ksm/run" >/dev/null
		echo "$pages_to_scan_was" | sudo tee "$ksm/pages_to_scan" >/dev/null
		;;
	balloon)
		log "balloon: booted VMs give their free pages back"
		tined_with --balloon
		bench -strategy cold,share "${idle[@]}" idle
		tined_with
		;;
	breaks)
		log "breaks: eviction, and running out of memory"
		bench -strategy share,uffd fork-evict
		bench pressure
		;;
	*)
		echo "no part $part" >&2
		exit 2
		;;
	esac
done
log "done: $out"
