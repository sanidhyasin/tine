# Step 0: boot one microVM by hand

**Goal:** start a Firecracker microVM yourself, get a shell inside it, and see the moving parts before writing any Go.

**Done when:** you have a root shell inside a VM you started, and you've paused and resumed it.

This follows Firecracker's [getting-started guide](https://github.com/firecracker-microvm/firecracker/blob/main/docs/getting-started.md), adapted to tine's folders. Everything except section 1 happens on the Linux server.

## 1. Get the server ready (once)

On your Mac, from the tine folder:

```bash
./scripts/dev-server.sh up
```

```bash
./scripts/dev-server.sh setup
```

```bash
./scripts/dev-server.sh ssh
```

`setup` copies the repo to the server and runs `scripts/host-setup.sh` there. It installs a few packages, checks that you can use KVM, mounts tine's data volume at `/srv/tine`, and puts everything for this walkthrough in `~/tine/artifacts/`:

| File | What it is |
|---|---|
| `bin/firecracker` | The VMM. One Firecracker process runs one microVM. |
| `vmlinux-*` | The guest's Linux kernel, uncompressed |
| `rootfs.ext4` | The guest's disk: an Ubuntu image |
| `guest_key` | An SSH key that can log in to the guest as root |

## 2. Terminal 1: start Firecracker

Open two SSH sessions to the server (run `./scripts/dev-server.sh ssh` in two Mac terminals). In the first one:

```bash
cd ~/tine/artifacts
sudo rm -f /tmp/firecracker.socket
sudo ./bin/firecracker --api-sock /tmp/firecracker.socket --enable-pci
```

Nothing seems to happen, and that's expected. Firecracker is now an empty VM waiting for instructions on a Unix socket. It doesn't boot anything until you describe the VM and tell it to start.

Leave this terminal open. The guest's console output will show up here.

## 3. Terminal 2: describe the VM and boot it

Everything in this section is plain HTTP requests with JSON bodies, sent to that socket. tine's Go manager will make exactly these calls in step 1.

### 3a. Networking on the host

```bash
cd ~/tine/artifacts

sudo ip link del tap0 2>/dev/null || true
sudo ip tuntap add dev tap0 mode tap
sudo ip addr add 172.16.0.1/30 dev tap0
sudo ip link set dev tap0 up

# Share the host's internet connection with the VM (NAT)
sudo sh -c "echo 1 > /proc/sys/net/ipv4/ip_forward"
sudo iptables -P FORWARD ACCEPT
HOST_IFACE=$(ip -j route list default | jq -r '.[0].dev')
sudo iptables -t nat -D POSTROUTING -o "$HOST_IFACE" -j MASQUERADE 2>/dev/null || true
sudo iptables -t nat -A POSTROUTING -o "$HOST_IFACE" -j MASQUERADE
```

A tap device is a virtual network cable. The host's end has the address `172.16.0.1`, and the other end gets plugged into the VM below. In step 3, every fork gets its own copy of this setup inside a separate network namespace.

### 3b. Describe the VM

First, a small helper so each call is one short line. It prints the HTTP status: `204` means OK, and anything else comes with a `fault_message` saying what's wrong.

```bash
fc() {
  sudo curl -sS -X PUT --unix-socket /tmp/firecracker.socket \
    -H 'Content-Type: application/json' --data "$2" \
    -w '%{http_code}\n' "http://localhost/$1"
}
```

Then describe the VM, one piece at a time:

```bash
# Where Firecracker writes its own log
touch firecracker.log
fc logger '{"log_path": "./firecracker.log", "level": "Debug", "show_level": true, "show_log_origin": true}'

# Size: 2 vCPUs and 1 GiB of memory (the default is 1 vCPU and 128 MiB)
fc machine-config '{"vcpu_count": 2, "mem_size_mib": 1024}'

# Which kernel to boot, and its command line
KERNEL=$(ls vmlinux-* | sort -V | tail -1)
fc boot-source "{\"kernel_image_path\": \"./$KERNEL\", \"boot_args\": \"console=ttyS0 reboot=k panic=1\"}"

# The disk
fc drives/rootfs '{"drive_id": "rootfs", "path_on_host": "./rootfs.ext4", "is_root_device": true, "is_read_only": false}'

# The network card, plugged into tap0. The guest works out its IP (172.16.0.2) from this MAC address.
fc network-interfaces/net1 '{"iface_id": "net1", "guest_mac": "06:00:AC:10:00:02", "host_dev_name": "tap0"}'
```

Paths are relative to where Firecracker was started, which is `~/tine/artifacts`.

### 3c. Boot it

```bash
fc actions '{"action_type": "InstanceStart"}'
```

Now look at terminal 1: the guest kernel's boot messages scroll past and end at a login prompt.

## 4. Get inside

```bash
SSH="ssh -i ./guest_key -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR root@172.16.0.2"

# Give the guest a route to the internet and a DNS server
$SSH "ip route add default via 172.16.0.1 dev eth0"
$SSH "echo 'nameserver 8.8.8.8' > /etc/resolv.conf; echo 'options single-request-reopen' >> /etc/resolv.conf"

$SSH
```

You're now root inside the microVM. Look around:

```bash
uname -r              # the guest's own kernel, not the host's
nproc && free -m      # 2 CPUs and about 1 GiB, as configured
ping -c 2 google.com  # internet works through the host's NAT
```

## 5. Experiments

These are short, and each one sets up a later step of the project.

### How much memory does the VM really use?

On the host (terminal 2, after `exit`ing the guest, or in a third SSH session):

```bash
ps -o pid,rss,cmd -C firecracker
```

`RSS` is how much memory the host has actually given the Firecracker process, in KiB. It's far below the 1 GiB you configured, because the host only hands out memory when the guest first touches it.

Now make the guest use some memory, and watch RSS on the host after each command:

```bash
# Inside the guest: write 400 MB into a RAM-backed folder
dd if=/dev/zero of=/dev/shm/fill bs=1M count=400
```

```bash
# On the host: RSS jumps by about 400 MB
ps -o pid,rss,cmd -C firecracker
```

```bash
# Inside the guest: free it again
rm /dev/shm/fill
```

```bash
# On the host: RSS does NOT go back down
ps -o pid,rss,cmd -C firecracker
```

The guest freed that memory, but the host has no way of knowing. Handing memory back to the host needs a "balloon" device, which is the feature involved in CVE-2026-122192. This matters for tine twice over: those pages end up in every snapshot, and they limit how many VMs fit on one host.

### Pause and resume

In a third SSH session on the server, start a ping to the guest and leave it running:

```bash
ping 172.16.0.2
```

In terminal 2, pause the VM:

```bash
sudo curl -sS -X PATCH --unix-socket /tmp/firecracker.socket \
  --data '{"state": "Paused"}' -w '%{http_code}\n' http://localhost/vm
```

The pings stop getting replies, because the guest's CPUs are frozen. Now resume it:

```bash
sudo curl -sS -X PATCH --unix-socket /tmp/firecracker.socket \
  --data '{"state": "Resumed"}' -w '%{http_code}\n' http://localhost/vm
```

The replies come back, and the guest never noticed it was frozen. Pausing is the first half of every snapshot: step 2 adds "save to disk" between pause and resume.

## 6. Shut down and clean up

Inside the guest, run `reboot`. Firecracker doesn't emulate power management, so a guest reboot makes the Firecracker process exit, and terminal 1 returns to the prompt.

Then on the host:

```bash
sudo ip link del tap0
```

If you're done for the day, stop the server from your Mac:

```bash
./scripts/dev-server.sh stop
```

## What you just learned, and where it comes back

| You did | Where it comes back |
|---|---|
| Configured a VM with HTTP calls on a socket | Step 1: the Go manager makes these calls |
| Plugged a tap device into the VM, with NAT | Step 3: one network namespace per fork |
| Saw memory handed out lazily and never given back | Steps 3–4: sharing memory between forks, and how many fit on a host |
| Paused and resumed a running VM | Step 2: pause, snapshot, resume |

## If something goes wrong

- **`/dev/kvm not found`:** the server wasn't launched with nested virtualization. Check `NestedVirt` in `./scripts/dev-server.sh status`.
- **An `fc` call returns something other than `204`:** read the `fault_message`. It's usually a wrong file path.
- **Firecracker fails to start because the socket already exists:** run `sudo rm -f /tmp/firecracker.socket`.
- **SSH to `172.16.0.2` hangs:** check `ip addr show tap0` on the host, and look at the guest's console in terminal 1.
- **Anything else:** read Firecracker's own log at `~/tine/artifacts/firecracker.log`.
