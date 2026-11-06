// Package netns gives every VM its own network: a Linux network namespace
// with the VM's tap device, connected to the host by a veth pair, with NAT to
// the internet and firewall rules around it.
//
//	host (root namespace)                       namespace tine-<id>
//	┌──────────────────────────┐                ┌───────────────────────────────────────┐
//	│ uplink ── internet       │                │ veth0  10.200.N.2/30                  │
//	│   ▲ NAT 10.200.0.0/16    │   veth pair    │   ▲ NAT 172.16.0.2 → 10.200.N.2       │
//	│ tine<N> 10.200.N.1/30 ───┼────────────────┼───┘                                   │
//	└──────────────────────────┘                │ tap0   172.16.0.1/30 ◄─► guest eth0   │
//	                                            │                          172.16.0.2   │
//	                                            └───────────────────────────────────────┘
//
// Inside every namespace the addresses are identical: the guest is always
// 172.16.0.2 behind tap0. Only the veth's addresses differ, by the VM's
// slot N. So VMs restored from one snapshot need no network changes in the
// guest at all.
//
// The firewall lets VMs reach the internet, and nothing else: not each
// other, not the host's own services, not private or link-local networks
// (which include the EC2 metadata service at 169.254.169.254), and no IPv6.
// Nothing outside can open connections into a VM.
//
// Everything that runs commands or touches the kernel needs Linux and root.
package netns

import (
	"encoding/binary"
	"fmt"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
)

const (
	// MaxSlots is how many VM networks fit in VMSubnet: one /30 each.
	MaxSlots = 1 << 14

	// NamespacePrefix starts the name of every VM's network namespace.
	NamespacePrefix = "tine-"
	// HostVethPrefix starts the name of every VM's host-side veth.
	HostVethPrefix = "tine"
	// NamespaceDir is where named network namespaces live, as with `ip netns`.
	NamespaceDir = "/var/run/netns"

	// Inside every VM's namespace:

	// TapDevice is the VM's tap device. Firecracker opens it by this name.
	TapDevice = "tap0"
	// NamespaceVeth is the namespace's end of the veth pair.
	NamespaceVeth = "veth0"
	// GuestMAC is the guest's MAC address. Firecracker's CI images also derive
	// their IP from it, and 06:00:AC:10:00:02 means 172.16.0.2.
	GuestMAC = "06:00:AC:10:00:02"
)

var (
	// VMSubnet holds every VM's veth addresses: slot N gets 10.200.0.0 + 4N, as a /30.
	VMSubnet = netip.MustParsePrefix("10.200.0.0/16")
	// GuestIP is the guest's address, the same in every VM.
	GuestIP = netip.MustParseAddr("172.16.0.2")
	// GatewayIP is tap0's address in the namespace: the guest's default gateway.
	GatewayIP = netip.MustParseAddr("172.16.0.1")

	// BlockedDestinations are networks VMs may never reach. They cover the
	// EC2 metadata service and VPC DNS (link-local), the host's VPC and any
	// other private network, carrier-grade NAT space and multicast.
	BlockedDestinations = []netip.Prefix{
		netip.MustParsePrefix("0.0.0.0/8"),
		netip.MustParsePrefix("10.0.0.0/8"),
		netip.MustParsePrefix("100.64.0.0/10"),
		netip.MustParsePrefix("127.0.0.0/8"),
		netip.MustParsePrefix("169.254.0.0/16"),
		netip.MustParsePrefix("172.16.0.0/12"),
		netip.MustParsePrefix("192.168.0.0/16"),
		netip.MustParsePrefix("224.0.0.0/4"),
		netip.MustParsePrefix("240.0.0.0/4"),
	}
)

// validID matches the VM IDs tine uses (the same rule as the jailer's).
var validID = regexp.MustCompile(`^[a-zA-Z0-9-]{1,64}$`)

// Owner is who may open a VM's tap device: the uid and gid its jailed
// Firecracker runs as.
type Owner struct {
	UID, GID int
}

// Network is one VM's network: its names and addresses.
type Network struct {
	// ID is the VM's ID.
	ID string
	// Slot picks the VM's veth addresses and host veth name: 0 to MaxSlots-1.
	Slot int
	// Namespace is the network namespace's name, e.g. "tine-vm-7f3k2q9d1a".
	Namespace string
	// HostVeth is the host's end of the veth pair, e.g. "tine42".
	HostVeth string
	// HostIP and NamespaceIP are the two ends of the veth pair (a /30).
	HostIP, NamespaceIP netip.Addr
}

// Plan works out a VM's network from its ID and slot. It doesn't create anything.
func Plan(id string, slot int) (Network, error) {
	if !validID.MatchString(id) {
		return Network{}, fmt.Errorf("network for %q: VM IDs must be 1-64 letters, digits and hyphens", id)
	}
	if slot < 0 || slot >= MaxSlots {
		return Network{}, fmt.Errorf("network for %s: slot %d is outside 0-%d", id, slot, MaxSlots-1)
	}
	base := VMSubnet.Addr().As4()
	offset := uint32(slot) * 4
	host := addToIPv4(base, offset+1)
	return Network{
		ID:          id,
		Slot:        slot,
		Namespace:   NamespacePrefix + id,
		HostVeth:    HostVethPrefix + strconv.Itoa(slot),
		HostIP:      host,
		NamespaceIP: host.Next(),
	}, nil
}

func addToIPv4(a [4]byte, n uint32) netip.Addr {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], binary.BigEndian.Uint32(a[:])+n)
	return netip.AddrFrom4(b)
}

// NamespacePath is the namespace's handle, which the jailer joins with --netns.
func (n Network) NamespacePath() string { return NamespaceDir + "/" + n.Namespace }

// HostCIDR is the host end's address with its /30.
func (n Network) HostCIDR() netip.Prefix { return netip.PrefixFrom(n.HostIP, 30) }

// NamespaceCIDR is the namespace end's address with its /30.
func (n Network) NamespaceCIDR() netip.Prefix { return netip.PrefixFrom(n.NamespaceIP, 30) }

// GuestKernelArgs configures the guest's network on its kernel command
// line, so it's up before anything in the guest runs.
func GuestKernelArgs() string {
	// ip=<client>::<gateway>:<netmask>::<device>:<autoconf>
	return fmt.Sprintf("ip=%s::%s:255.255.255.252::eth0:off", GuestIP, GatewayIP)
}

// HostRuleset is the nftables script for the host's "inet tine" table. It
// replaces the whole table in one transaction, so it's safe to apply again.
func HostRuleset() string {
	vms := HostVethPrefix + "*"
	blocked := make([]string, len(BlockedDestinations))
	for i, p := range BlockedDestinations {
		blocked[i] = p.String()
	}
	return fmt.Sprintf(`# tine: firewall and NAT for VM networks. Managed by tined; changes are overwritten.
add table inet tine
delete table inet tine
table inet tine {
	chain input {
		type filter hook input priority filter; policy accept;
		iifname "%[1]s" ct state established,related accept
		iifname "%[1]s" counter drop comment "VMs can't reach the host's own services"
	}

	chain forward {
		type filter hook forward priority filter; policy accept;
		iifname "%[1]s" meta nfproto ipv6 counter drop comment "no IPv6 for VMs"
		iifname "%[1]s" oifname "%[1]s" counter drop comment "no traffic between VMs"
		iifname "%[1]s" ip daddr { %[2]s } counter drop comment "no private, link-local or metadata addresses"
		oifname "%[1]s" ct state established,related accept
		oifname "%[1]s" counter drop comment "nothing opens connections into VMs"
	}

	chain postrouting {
		type nat hook postrouting priority srcnat; policy accept;
		ip saddr %[3]s oifname != "%[1]s" masquerade
	}
}
`, vms, strings.Join(blocked, ", "), VMSubnet)
}

// NamespaceRuleset is the nftables script for the "inet tine" table inside
// a VM's namespace: NAT the guest to the veth address, forward only the
// guest's own traffic out, and only replies back in.
func NamespaceRuleset() string {
	return fmt.Sprintf(`table inet tine {
	chain input {
		type filter hook input priority filter; policy accept;
		iifname "%[1]s" ct state established,related accept
		iifname "%[1]s" meta l4proto icmp accept
		iifname "%[1]s" counter drop comment "the guest can't reach anything in its namespace"
	}

	chain forward {
		type filter hook forward priority filter; policy drop;
		iifname "%[1]s" oifname "%[2]s" ip saddr %[3]s accept
		iifname "%[2]s" oifname "%[1]s" ct state established,related accept
	}

	chain postrouting {
		type nat hook postrouting priority srcnat; policy accept;
		oifname "%[2]s" ip saddr %[3]s masquerade
	}
}
`, TapDevice, NamespaceVeth, GuestIP)
}
