# The "python" image: Ubuntu 24.04 with Python, pytest and git, for the
# best-of-N demo (examples/best-of-n). Build it on the server with:
#
#   sudo tined image build python -f images/python/Dockerfile --kernel ~/tine/artifacts/vmlinux-6.18.56
#
# A tine image boots with systemd as init (tine-agent starts as a systemd
# service), so systemd-sysv is the one thing every Dockerfile for tine needs.
FROM ubuntu:24.04

RUN apt-get update \
 && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      systemd systemd-sysv udev iproute2 iputils-ping ca-certificates curl \
      python3 python3-pip python3-venv python3-pytest git \
 && apt-get clean \
 && rm -rf /var/lib/apt/lists/*

# Nothing in the guest needs a login prompt or the network managers a
# container image might bring.
RUN systemctl mask getty@tty1.service serial-getty@ttyS0.service systemd-networkd.service systemd-resolved.service 2>/dev/null || true

# pip installs into the system Python, as it's the sandbox's own. (A file,
# not ENV: only the filesystem goes into a tine image.)
RUN printf '[global]\nbreak-system-packages = true\n' > /etc/pip.conf
