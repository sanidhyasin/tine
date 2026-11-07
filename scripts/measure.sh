#!/usr/bin/env bash
# Measures tine's numbers against the running tined: the "Measure" lists of
# steps 1 and 2 in docs/PLAN.md. Sections, in the order they run:
#
#   cold      create (booted, --cold) and destroy, one at a time        CREATES (100)
#   template  create from base's template and destroy, one at a time    CREATES (100)
#   exec      run `true`: through the API, the CLI, and inside the guest EXECS (200)
#   memory    host memory of idle VMs, booted and from the template      IDLE_VMS (5 of each)
#   rollback  roll a VM back to a snapshot                               ROLLBACKS (20)
#   snapshot  snapshot pause and total time at 256 MiB, 1 GiB and 4 GiB   SNAPSHOTS (5 per size)
#   fork      fork a 1-vCPU, 256 MiB VM into 1, 10 and 30, with each        FORKS (5, 3, 2)
#             child's RSS and PSS; 30 is the most the dev server's
#             vCPU limit allows
#
# Each section first waits for the disk to settle: a snapshot's memory is
# written to disk in the background afterwards, and that slows down
# anything else that reads or writes the disk.
#
# Runs on the dev server, as a member of the tine group, with sudo to read
# VMs' memory (tine's cgroups are root-only). From the Mac:
#   make measure                               every section
#   make measure SECTIONS="template snapshot"  some
# Or on the server:
#   scripts/measure.sh [SECTION...]
set -euo pipefail

all_sections=(cold template exec memory rollback snapshot fork)
sections=("$@")
if ((${#sections[@]} == 0)); then
	sections=("${all_sections[@]}")
fi
creates=${CREATES:-100}
execs=${EXECS:-200}
idle_vms=${IDLE_VMS:-5}
rollbacks=${ROLLBACKS:-20}
snapshots=${SNAPSHOTS:-5}
socket=${TINE_SOCKET:-/run/tine/tine.sock}

work=$(mktemp -d)
live=()
finish() {
	if ((${#live[@]} > 0)); then
		tine destroy "${live[@]}" >/dev/null || true
	fi
	rm -r "$work"
}
trap finish EXIT

# stats FILE prints "p50 · p99 · max · mean (n)" for a file of numbers in ms.
stats() {
	sort -n "$1" | awk '
		function at(p) { return v[int(p * (NR - 1) + 0.5) + 1] }
		function ms(x) {
			if (x >= 1000) return sprintf("%.2f s", x / 1000)
			if (x >= 10) return sprintf("%.0f ms", x)
			if (x >= 1) return sprintf("%.1f ms", x)
			return sprintf("%.2f ms", x)
		}
		{ v[NR] = $1; sum += $1 }
		END {
			if (NR == 0) { print "no samples"; exit }
			printf "p50 %-8s  p99 %-8s  max %-8s  mean %-8s  (n=%d)\n", ms(at(0.5)), ms(at(0.99)), ms(v[NR]), ms(sum / NR), NR
		}'
}
mib() { sort -n "$1" | awk '{ v[NR] = $1; s += $1 } END { printf "mean %.0f MiB  (min %.0f, max %.0f, n=%d)\n", s / NR, v[1], v[NR], NR }'; }
now_ms() { echo $(($(date +%s%N) / 1000000)); }
row() { printf "    %-12s %s\n" "$1" "$(stats "$2")"; }

# create_vm ARGS... makes a VM, sets $vm_id, and remembers it for the cleanup on exit.
create_vm() {
	vm_id=$(tine create -q --ttl 30m "$@")
	live+=("$vm_id")
}
destroy_vm() {
	tine destroy "$1" >/dev/null
	local keep=() v
	for v in "${live[@]}"; do [[ $v == "$1" ]] || keep+=("$v"); done
	live=("${keep[@]+"${keep[@]}"}")
}

# quiet waits, up to 3 minutes, until the kernel has written dirty pages out.
quiet() {
	local i
	for ((i = 0; i < 180; i++)); do
		if (($(awk '/^(Dirty|Writeback):/ { s += $2 } END { print s }' /proc/meminfo) < 32768)); then
			return 0
		fi
		sleep 1
	done
	echo "measure: the disk is still busy after 3 minutes; carrying on" >&2
}

# creates KIND N STAGES... creates and destroys N VMs, one at a time, and
# prints each stage's timings. KIND is cold or template.
creates() {
	local kind=$1 n=$2 i vm id start end s
	shift 2
	local stages=("$@" total)
	local flag=()
	[[ $kind == cold ]] && flag=(--cold)
	for ((i = 1; i <= n; i++)); do
		start=$(now_ms)
		vm=$(tine --json create --ttl 30m "${flag[@]+"${flag[@]}"}")
		end=$(now_ms)
		id=$(jq -r .id <<<"$vm")
		live+=("$id")
		if [[ $(jq -r .source <<<"$vm") != "$kind" ]]; then
			echo "measure: $id started $(jq -r .source <<<"$vm"), not $kind" >&2
			exit 1
		fi
		echo $((end - start)) >>"$work/$kind-cli"
		for s in "${stages[@]}"; do
			jq -r ".timings_ms.$s // empty" <<<"$vm" >>"$work/$kind-$s"
		done
		tine --json destroy "$id" | jq -r '.[0].timings_ms.destroy' >>"$work/$kind-destroy"
		live=()
		if ((i % 25 == 0)); then echo "    $i/$n"; fi
	done
	for s in "${stages[@]}"; do row "$s" "$work/$kind-$s"; done
	row "via CLI" "$work/$kind-cli"
	row "destroy" "$work/$kind-destroy"
}

section_cold() {
	echo "==> Booting $creates VMs (--cold), one at a time, and destroying each (agent = until the guest's agent answers)"
	creates cold "$creates" allocate network jail start configure agent sync
}

section_template() {
	echo "==> Restoring $creates VMs from base's template, one at a time, and destroying each"
	creates template "$creates" allocate network jail start restore agent sync
}

section_exec() {
	echo "==> Running \`true\` $execs times in one VM"
	create_vm
	local id=$vm_id i start
	sleep 5
	for ((i = 1; i <= execs; i++)); do
		curl -sS --fail --unix-socket "$socket" -H 'Content-Type: application/json' \
			-d '{"args":["true"]}' -o "$work/frames" -w '%{time_total}\n' \
			"http://tined/v1/vms/$id/exec" | awk '{ print $1 * 1000 }' >>"$work/exec-api"
		tail -n 1 "$work/frames" | jq -e -r 'select(.exit.code == 0) | .exit.duration_ms' >>"$work/exec-guest" ||
			{ echo "measure: exec failed: $(cat "$work/frames")" >&2; exit 1; }
	done
	for ((i = 1; i <= execs / 4; i++)); do
		start=$(now_ms)
		tine exec "$id" true
		echo $(($(now_ms) - start)) >>"$work/exec-cli"
	done
	destroy_vm "$id"
	row "API" "$work/exec-api"
	row "CLI" "$work/exec-cli"
	row "in guest" "$work/exec-guest"
}

section_memory() {
	echo "==> Starting $idle_vms default-sized VMs booted and $idle_vms from the template, then leaving them idle for 30s"
	local ids=() kinds=() i id kind cg pid status
	for ((i = 0; i < idle_vms; i++)); do
		create_vm --cold
		ids+=("$vm_id") kinds+=(cold)
		create_vm
		ids+=("$vm_id") kinds+=(template)
	done
	sleep 30
	for i in "${!ids[@]}"; do
		id=${ids[$i]} kind=${kinds[$i]}
		cg=/sys/fs/cgroup/tine/$id
		pid=$(sudo head -n 1 "$cg/cgroup.procs")
		status=$(sudo cat "/proc/$pid/status")
		awk '/^VmRSS:/ { print $2 / 1024 }' <<<"$status" >>"$work/rss-$kind"
		awk '/^RssAnon:/ { print $2 / 1024 }' <<<"$status" >>"$work/anon-$kind"
		awk '/^RssFile:/ { print $2 / 1024 }' <<<"$status" >>"$work/file-$kind"
		sudo cat "$cg/memory.current" | awk '{ print $1 / 1048576 }' >>"$work/cgroup-$kind"
	done
	for id in "${ids[@]}"; do destroy_vm "$id"; done
	for kind in cold template; do
		echo "    $kind (2 vCPUs, 1 GiB, 30s after starting):"
		printf "      %-28s %s\n" "Firecracker RSS" "$(mib "$work/rss-$kind")"
		printf "      %-28s %s\n" "  private (anonymous)" "$(mib "$work/anon-$kind")"
		printf "      %-28s %s\n" "  mapped from a file" "$(mib "$work/file-$kind")"
		printf "      %-28s %s\n" "charged to its cgroup" "$(mib "$work/cgroup-$kind")"
	done
}

section_rollback() {
	echo "==> Rolling one VM back to a snapshot $rollbacks times"
	create_vm
	local id=$vm_id snap i
	snap=$(tine snapshot -q "$id")
	for ((i = 1; i <= rollbacks; i++)); do
		tine exec "$id" -- sh -c "echo $i > /root/changed"
		tine --json rollback "$id" --to "$snap" | jq -r .timings_ms.rollback >>"$work/rollback"
	done
	destroy_vm "$id"
	tine snapshot rm "$snap" >/dev/null 2>&1 || true # or the reaper will
	row "rollback" "$work/rollback"
}

section_snapshot() {
	echo "==> Snapshotting VMs of 256 MiB, 1 GiB and 4 GiB, $snapshots times each (paused = how long the VM was frozen)"
	local size id i snap
	for size in 256 1024 4096; do
		quiet
		create_vm --cold --memory "${size}M"
		id=$vm_id
		sleep 5
		for ((i = 1; i <= snapshots; i++)); do
			snap=$(tine --json snapshot "$id")
			jq -r .timings_ms.paused <<<"$snap" >>"$work/snap-$size-paused"
			jq -r '.timings_ms.total - .timings_ms.wait' <<<"$snap" >>"$work/snap-$size-total"
			jq -r .timings_ms.wait <<<"$snap" >>"$work/snap-$size-wait"
			tine snapshot rm "$(jq -r .id <<<"$snap")" >/dev/null
		done
		destroy_vm "$id"
		echo "    $size MiB:"
		row "  paused" "$work/snap-$size-paused"
		row "  total" "$work/snap-$size-total"
		row "  waited" "$work/snap-$size-wait"
	done
	echo "    (total leaves out the wait for the VM's previous snapshot to reach the disk, shown as waited)"
}

section_fork() {
	echo "==> Forking a fresh 1-vCPU, 256 MiB VM into 1, 10 and 30 children (paused = how long the parent was frozen)"
	local n reps i parent res kids
	for n in 1 10 30; do
		case $n in 1) reps=${FORKS:-5} ;; 10) reps=${FORKS:-3} ;; *) reps=${FORKS:-2} ;; esac
		for ((i = 1; i <= reps; i++)); do
			quiet
			create_vm --cold --vcpus 1 --memory 256M
			parent=$vm_id
			sleep 3
			res=$(tine --json fork "$parent" -n "$n" --ttl 30m)
			kids=$(jq -r '.children[].vm.id // empty' <<<"$res")
			for k in $kids; do live+=("$k"); done
			jq -r .timings_ms.total <<<"$res" >>"$work/fork-$n-total"
			jq -r .timings_ms.paused <<<"$res" >>"$work/fork-$n-paused"
			jq -r .timings_ms.children <<<"$res" >>"$work/fork-$n-children"
			if [[ $n == 30 ]]; then
				sleep 10
				tine --json ls --tree | jq -r --arg p "$parent" '.[] | select(.parent_id == $p) | .memory | "\(.rss_bytes / 1048576) \(.pss_bytes / 1048576)"' >"$work/fork-mem"
				awk '{ print $1 }' "$work/fork-mem" >>"$work/fork-rss"
				awk '{ print $2 }' "$work/fork-mem" >>"$work/fork-pss"
			fi
			for k in $kids "$parent"; do destroy_vm "$k"; done
		done
		echo "    $n children:"
		row "  total" "$work/fork-$n-total"
		row "  paused" "$work/fork-$n-paused"
		row "  children" "$work/fork-$n-children"
	done
	echo "    each of 30 idle children, 10s after the fork:"
	printf "      %-12s %s\n" "RSS" "$(mib "$work/fork-rss")"
	printf "      %-12s %s\n" "PSS" "$(mib "$work/fork-pss")"
}

for s in "${sections[@]}"; do
	if ! declare -F "section_$s" >/dev/null; then
		echo "measure: no section $s; there are: ${all_sections[*]}" >&2
		exit 1
	fi
done
tine version | sed 's/^/    /'
if (($(tine --json ls | jq length) > 0)); then
	echo "measure: other VMs are running; stop them first so they don't skew the numbers" >&2
	exit 1
fi
for s in "${sections[@]}"; do
	quiet
	"section_$s"
done
