#!/usr/bin/env bash
# Measures step 1's numbers against the running tined (docs/PLAN.md, step 1, "Measure"):
#
#   1. Cold-boot create latency: VMs created one at a time, so nothing else is
#      running, with tined's own breakdown of each stage. Each is destroyed
#      straight away, which measures destroy too.
#   2. Exec round trip for `true`: through the API (curl on the socket), the
#      tine CLI, and as timed inside the guest.
#   3. Host memory per idle VM: a few default-sized VMs, left to settle.
#
# Runs on the dev server, as a member of the tine group, with sudo to read
# VMs' memory (tine's cgroups are root-only). From the Mac:
#   make measure
# Or on the server:
#   scripts/measure.sh [CREATES] [EXECS]        (defaults: 100 and 200)
set -euo pipefail

creates=${1:-100}
execs=${2:-200}
idle_vms=${IDLE_VMS:-5}
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
# create_vm makes a VM, sets $vm_id, and remembers it for the cleanup on exit.
create_vm() {
	vm_id=$(tine create -q --ttl 30m)
	live+=("$vm_id")
}
destroy_vm() {
	tine destroy "$1" >/dev/null
	local keep=() v
	for v in "${live[@]}"; do [[ $v == "$1" ]] || keep+=("$v"); done
	live=("${keep[@]+"${keep[@]}"}")
}

tine version | sed 's/^/    /'
if (($(tine --json ls | jq length) > 0)); then
	echo "measure: other VMs are running; stop them first so they don't skew the numbers" >&2
	exit 1
fi

# 1. Cold-boot create and destroy, one at a time.
stages=(allocate network jail start configure agent total)
echo "==> Creating and destroying $creates VMs, one at a time"
for ((i = 1; i <= creates; i++)); do
	start=$(now_ms)
	vm=$(tine --json create --ttl 30m)
	end=$(now_ms)
	id=$(jq -r .id <<<"$vm")
	live+=("$id")
	echo $((end - start)) >>"$work/create-cli"
	for s in "${stages[@]}"; do
		jq -r ".timings_ms.$s // empty" <<<"$vm" >>"$work/create-$s"
	done
	tine --json destroy "$id" | jq -r '.[0].timings_ms.destroy' >>"$work/destroy"
	live=()
	if ((i % 25 == 0)); then echo "    $i/$creates"; fi
done
echo "Cold-boot create, one at a time (tined's own timings; agent = boot until the agent answers):"
for s in "${stages[@]}"; do printf "    %-10s %s\n" "$s" "$(stats "$work/create-$s")"; done
printf "    %-10s %s\n" "via CLI" "$(stats "$work/create-cli")"
printf "    %-10s %s\n" "destroy" "$(stats "$work/destroy")"

# 2. Exec round trip, in a VM that has finished booting.
echo "==> Running \`true\` $execs times in one VM"
create_vm
id=$vm_id
sleep 10
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
echo "Exec \`true\` in a booted VM:"
printf "    %-10s %s\n" "API" "$(stats "$work/exec-api")"
printf "    %-10s %s\n" "CLI" "$(stats "$work/exec-cli")"
printf "    %-10s %s\n" "in guest" "$(stats "$work/exec-guest")"

# 3. Memory per idle VM: default size, booted, then left alone.
echo "==> Booting $idle_vms default-sized VMs and leaving them idle for 30s"
ids=()
for ((i = 0; i < idle_vms; i++)); do
	create_vm
	ids+=("$vm_id")
done
sleep 30
size=$(tine --json inspect "${ids[0]}" | jq -r '"\(.vcpus) vCPUs, \(.memory_mib) MiB"')
for id in "${ids[@]}"; do
	cg=/sys/fs/cgroup/tine/$id
	pid=$(sudo head -n 1 "$cg/cgroup.procs")
	status=$(sudo cat "/proc/$pid/status")
	awk '/^VmRSS:/ { print $2 / 1024 }' <<<"$status" >>"$work/rss"
	awk '/^RssAnon:/ { print $2 / 1024 }' <<<"$status" >>"$work/rss-anon"
	sudo cat "$cg/memory.current" | awk '{ print $1 / 1048576 }' >>"$work/cgroup"
done
for id in "${ids[@]}"; do destroy_vm "$id"; done
echo "Host memory per idle VM ($size, 30s after boot):"
printf "    %-26s %s\n" "Firecracker RSS" "$(mib "$work/rss")"
printf "    %-26s %s\n" "  of which anonymous" "$(mib "$work/rss-anon")"
printf "    %-26s %s\n" "VM's cgroup (incl. cache)" "$(mib "$work/cgroup")"
