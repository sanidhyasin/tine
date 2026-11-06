#!/usr/bin/env bash
# Prepares a Linux host to run Firecracker microVMs for tine.
#
# Run this ON THE LINUX SERVER, not on your Mac:
#   cd ~/tine && ./scripts/host-setup.sh
#
# What it does:
#   1. Installs the packages it needs (Ubuntu/Debian).
#   2. Checks that KVM exists and that you can use it.
#   3. Downloads Firecracker into artifacts/bin/.
#   4. Downloads a guest kernel and an Ubuntu disk image from Firecracker's CI,
#      adds an SSH key to the image, and writes artifacts/rootfs.ext4.
#
# It's safe to re-run: anything already done is skipped.
# Based on https://github.com/firecracker-microvm/firecracker/blob/main/docs/getting-started.md

set -euo pipefail

# v1.16.2 / v1.17.0 or later is needed for the CVE-2026-122192 fix.
FC_VERSION="${FC_VERSION:-v1.17.0}"
ROOTFS_SIZE="${ROOTFS_SIZE:-2G}"
REPO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ART="${ARTIFACTS_DIR:-$REPO_DIR/artifacts}"
ARCH="$(uname -m)"
S3="https://s3.amazonaws.com/spec.ccfc.min"

say() { printf '\n\033[1m==> %s\033[0m\n' "$*"; }
die() { printf '\033[31mERROR: %s\033[0m\n' "$*" >&2; exit 1; }

[ "$(uname -s)" = "Linux" ] || die "Run this on the Linux dev server, not on macOS."
case "$ARCH" in x86_64|aarch64) ;; *) die "Unsupported architecture: $ARCH" ;; esac
command -v apt-get >/dev/null || die "This script expects Ubuntu or Debian (apt-get)."

# --- 1. Packages ------------------------------------------------------------
say "Installing packages"
sudo apt-get -o DPkg::Lock::Timeout=300 update -qq
sudo DEBIAN_FRONTEND=noninteractive apt-get -o DPkg::Lock::Timeout=300 install -y -qq \
  curl jq squashfs-tools e2fsprogs acl iproute2 iptables openssh-client >/dev/null
echo "OK"

# --- 2. KVM -----------------------------------------------------------------
say "Checking KVM"
if [ ! -e /dev/kvm ]; then
  die "/dev/kvm not found. On EC2, use a c8i/m8i/r8i instance launched with
       NestedVirtualization=enabled (scripts/dev-server.sh does this), or a .metal instance."
fi
# Access for this session (ACL), and for future logins (kvm group).
sudo setfacl -m "u:${USER}:rw" /dev/kvm
if [ "$(stat -c '%G' /dev/kvm)" = "kvm" ] && ! id -nG "$USER" | grep -qw kvm; then
  sudo usermod -aG kvm "$USER"
fi
{ [ -r /dev/kvm ] && [ -w /dev/kvm ]; } || die "No read/write access to /dev/kvm"
echo "KVM OK ($(lsmod | awk '/^kvm_(intel|amd)/ {print $1; exit}'))"

mkdir -p "$ART/bin"
cd "$ART"

# --- 3. Firecracker ---------------------------------------------------------
say "Firecracker $FC_VERSION"
if [ -x bin/firecracker ] && bin/firecracker --version | grep -q "${FC_VERSION#v}"; then
  echo "Already installed"
else
  tmp="$(mktemp -d)"
  curl -fsSL "https://github.com/firecracker-microvm/firecracker/releases/download/${FC_VERSION}/firecracker-${FC_VERSION}-${ARCH}.tgz" \
    | tar -xz -C "$tmp"
  rel="$tmp/release-${FC_VERSION}-${ARCH}"
  install -m 0755 "$rel/firecracker-${FC_VERSION}-${ARCH}" bin/firecracker
  [ -f "$rel/jailer-${FC_VERSION}-${ARCH}" ] && install -m 0755 "$rel/jailer-${FC_VERSION}-${ARCH}" bin/jailer
  rm -rf "$tmp"
fi
bin/firecracker --version | head -1

# --- 4. Guest kernel and rootfs ---------------------------------------------
say "Guest kernel and rootfs (from Firecracker's CI artifacts)"

ci_prefix() {
  curl -fsSL "$S3?list-type=2&prefix=firecracker-ci/&delimiter=/" \
    | grep -oP "(?<=<Prefix>)firecracker-ci/[0-9]{8}-[^/]+/(?=</Prefix>)" \
    | sort | tail -1
}

if ls vmlinux-* >/dev/null 2>&1; then
  echo "Kernel already downloaded"
else
  CI_PREFIX="$(ci_prefix)"
  [ -n "$CI_PREFIX" ] || die "Couldn't list Firecracker CI artifacts"
  kernel_key="$(curl -fsSL "$S3?list-type=2&prefix=${CI_PREFIX}${ARCH}/vmlinux-" \
    | grep -oP "(?<=<Key>)(${CI_PREFIX}${ARCH}/vmlinux-[0-9]+\.[0-9]+\.[0-9]{1,3})(?=</Key>)" \
    | sort -V | tail -1)"
  [ -n "$kernel_key" ] || die "Couldn't find a guest kernel under ${CI_PREFIX}${ARCH}/"
  curl -fsSL -o "$(basename "$kernel_key")" "$S3/$kernel_key"
fi
KERNEL="$(ls vmlinux-* | sort -V | tail -1)"

if [ -f rootfs.ext4 ]; then
  echo "Rootfs already built"
else
  CI_PREFIX="${CI_PREFIX:-$(ci_prefix)}"
  ubuntu_key="$(curl -fsSL "$S3?list-type=2&prefix=${CI_PREFIX}${ARCH}/ubuntu-" \
    | grep -oP "(?<=<Key>)(${CI_PREFIX}${ARCH}/ubuntu-[0-9]+\.[0-9]+\.squashfs)(?=</Key>)" \
    | sort -V | tail -1)"
  [ -n "$ubuntu_key" ] || die "Couldn't find an Ubuntu rootfs under ${CI_PREFIX}${ARCH}/"
  basename "$ubuntu_key" .squashfs | grep -oE '[0-9]+\.[0-9]+' > rootfs.version
  curl -fsSL -o ubuntu.squashfs "$S3/$ubuntu_key"

  # The CI image has no SSH keys, so make one and let it log in as root.
  [ -f guest_key ] || ssh-keygen -q -t ed25519 -f guest_key -N "" -C tine-guest
  sudo rm -rf squashfs-root
  unsquashfs -q ubuntu.squashfs >/dev/null
  mkdir -p squashfs-root/root/.ssh
  cp guest_key.pub squashfs-root/root/.ssh/authorized_keys

  # Pack it into an ext4 disk image.
  sudo chown -R root:root squashfs-root
  truncate -s "$ROOTFS_SIZE" rootfs.ext4
  sudo mkfs.ext4 -q -d squashfs-root -F rootfs.ext4
  sudo rm -rf squashfs-root ubuntu.squashfs
fi
e2fsck -fn rootfs.ext4 >/dev/null 2>&1 || die "rootfs.ext4 isn't a valid ext4 image. Delete it and re-run."

# --- Done -------------------------------------------------------------------
say "Ready"
cat <<EOF
Firecracker:  $ART/bin/firecracker
Kernel:       $ART/$KERNEL
Rootfs:       $ART/rootfs.ext4 (Ubuntu $(cat rootfs.version 2>/dev/null || echo '?'), $ROOTFS_SIZE)
Guest key:    $ART/guest_key

Next: docs/step-0-boot-one-vm.md
EOF
