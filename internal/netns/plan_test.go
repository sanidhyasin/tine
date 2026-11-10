package netns

import (
	"net/netip"
	"strings"
	"testing"
)

func TestPlanAddresses(t *testing.T) {
	tests := []struct {
		slot             int
		hostIP, nsIP     string
		hostVeth         string
		hostCIDR, nsCIDR string
	}{
		{0, "10.200.0.1", "10.200.0.2", "tine0", "10.200.0.1/30", "10.200.0.2/30"},
		{1, "10.200.0.5", "10.200.0.6", "tine1", "10.200.0.5/30", "10.200.0.6/30"},
		{64, "10.200.1.1", "10.200.1.2", "tine64", "10.200.1.1/30", "10.200.1.2/30"},
		{MaxSlots - 1, "10.200.255.253", "10.200.255.254", "tine16383", "10.200.255.253/30", "10.200.255.254/30"},
	}
	for _, tt := range tests {
		n, err := Plan("vm-abc", tt.slot)
		if err != nil {
			t.Fatalf("Plan(slot %d): %v", tt.slot, err)
		}
		if n.HostIP.String() != tt.hostIP || n.NamespaceIP.String() != tt.nsIP || n.HostVeth != tt.hostVeth {
			t.Errorf("slot %d: host %s, namespace %s, veth %s; want %s, %s, %s",
				tt.slot, n.HostIP, n.NamespaceIP, n.HostVeth, tt.hostIP, tt.nsIP, tt.hostVeth)
		}
		if n.HostCIDR().String() != tt.hostCIDR || n.NamespaceCIDR().String() != tt.nsCIDR {
			t.Errorf("slot %d: CIDRs %s and %s, want %s and %s", tt.slot, n.HostCIDR(), n.NamespaceCIDR(), tt.hostCIDR, tt.nsCIDR)
		}
		if !VMSubnet.Contains(n.HostIP) || !VMSubnet.Contains(n.NamespaceIP) {
			t.Errorf("slot %d: addresses outside %s", tt.slot, VMSubnet)
		}
		if len(n.HostVeth) > 15 {
			t.Errorf("slot %d: veth name %q is longer than Linux's 15-character limit", tt.slot, n.HostVeth)
		}
	}
}

func TestPlanNoTwoSlotsOverlap(t *testing.T) {
	seen := map[netip.Addr]int{}
	for slot := 0; slot < MaxSlots; slot++ {
		n, err := Plan("vm-x", slot)
		if err != nil {
			t.Fatal(err)
		}
		for _, a := range []netip.Addr{n.HostIP, n.NamespaceIP} {
			if other, dup := seen[a]; dup {
				t.Fatalf("%s is used by slots %d and %d", a, other, slot)
			}
			seen[a] = slot
		}
	}
}

func TestPlanNames(t *testing.T) {
	n, err := Plan("vm-7f3k2q9d1a", 3)
	if err != nil {
		t.Fatal(err)
	}
	if n.ID != "vm-7f3k2q9d1a" || n.Slot != 3 || n.Namespace != "tine-vm-7f3k2q9d1a" {
		t.Errorf("Plan = %+v", n)
	}
	if got, want := n.NamespacePath(), "/var/run/netns/tine-vm-7f3k2q9d1a"; got != want {
		t.Errorf("NamespacePath = %s, want %s", got, want)
	}
}

func TestPlanRejects(t *testing.T) {
	for _, tc := range []struct {
		id   string
		slot int
	}{
		{"", 0}, {"../etc", 0}, {"a b", 0}, {strings.Repeat("a", 65), 0},
		{"vm-ok", -1}, {"vm-ok", MaxSlots},
	} {
		if _, err := Plan(tc.id, tc.slot); err == nil {
			t.Errorf("Plan(%q, %d): want an error", tc.id, tc.slot)
		}
	}
}

func TestGuestKernelArgs(t *testing.T) {
	if got, want := GuestKernelArgs(), "ip=172.16.0.2::172.16.0.1:255.255.255.252::eth0:off"; got != want {
		t.Errorf("GuestKernelArgs = %q, want %q", got, want)
	}
	if !netip.PrefixFrom(GatewayIP, 30).Contains(GuestIP) {
		t.Error("the guest and its gateway aren't in one /30")
	}
}

// The firewall's job is spelled out in rules; check each promise has its rule.
func TestHostRuleset(t *testing.T) {
	rs := HostRuleset()
	for _, want := range []string{
		"add table inet tine\ndelete table inet tine\n", // replaces the table atomically
		`iifname "tine*" counter drop comment "VMs can't reach the host's own services"`,
		`iifname "tine*" meta nfproto ipv6 counter drop`,
		`iifname "tine*" oifname "tine*" counter drop comment "no traffic between VMs"`,
		`oifname "tine*" counter drop comment "nothing opens connections into VMs"`,
		`ip saddr 10.200.0.0/16 oifname != "tine*" masquerade`,
	} {
		if !strings.Contains(rs, want) {
			t.Errorf("host ruleset is missing %q", want)
		}
	}
	for _, blocked := range []string{"169.254.0.0/16", "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "100.64.0.0/10"} {
		if !strings.Contains(rs, blocked) {
			t.Errorf("host ruleset doesn't block %s", blocked)
		}
	}
	// Established VM traffic must be accepted before the drop rules.
	if strings.Index(rs, `oifname "tine*" ct state established,related accept`) > strings.Index(rs, `nothing opens connections into VMs`) {
		t.Error("replies to VMs would be dropped: accept must come before the drop")
	}
}

func TestBlockedDestinationsCoverTheMetadataService(t *testing.T) {
	for _, addr := range []string{"169.254.169.254", "169.254.169.253", "172.31.0.2", "10.200.0.5"} {
		a := netip.MustParseAddr(addr)
		covered := false
		for _, p := range BlockedDestinations {
			if p.Contains(a) {
				covered = true
			}
		}
		if !covered {
			t.Errorf("%s isn't blocked", addr)
		}
	}
	if VMSubnet.Contains(GuestIP) {
		t.Error("the guest's address overlaps the VM subnet")
	}
}

func TestNamespaceRuleset(t *testing.T) {
	rs := NamespaceRuleset()
	for _, want := range []string{
		"type filter hook forward priority filter; policy drop;",
		`iifname "tap0" oifname "veth0" ip saddr 172.16.0.2 accept`,
		`iifname "veth0" oifname "tap0" ct state established,related accept`,
		`oifname "veth0" ip saddr 172.16.0.2 masquerade`,
		`iifname "tap0" counter drop`,
	} {
		if !strings.Contains(rs, want) {
			t.Errorf("namespace ruleset is missing %q", want)
		}
	}
}

func TestOfflineRuleset(t *testing.T) {
	rs := OfflineRuleset()
	for _, want := range []string{
		"type filter hook forward priority filter; policy drop;",
		`iifname "tap0" counter drop`,
	} {
		if !strings.Contains(rs, want) {
			t.Errorf("offline ruleset is missing %q", want)
		}
	}
	// Nothing leaves: no NAT, and nothing forwarded.
	for _, never := range []string{"masquerade", "veth0", "postrouting"} {
		if strings.Contains(rs, never) {
			t.Errorf("offline ruleset has %q:\n%s", never, rs)
		}
	}
	// The input chain's policy, replies, and ICMP to the gateway: as online.
	if strings.Count(rs, "accept") != 3 {
		t.Errorf("offline ruleset accepts more than the namespace's input chain does online:\n%s", rs)
	}
}

func TestOfflinePlan(t *testing.T) {
	n, err := OfflinePlan("vm-x", 7)
	if err != nil {
		t.Fatal(err)
	}
	online, _ := Plan("vm-x", 7)
	if !n.Offline || online.Offline {
		t.Errorf("Offline = %v, %v; want true for OfflinePlan only", n.Offline, online.Offline)
	}
	if n.Namespace != online.Namespace || n.Slot != online.Slot {
		t.Errorf("an offline network's namespace or slot differ: %+v vs %+v", n, online)
	}
	if _, err := OfflinePlan("bad id!", 7); err == nil {
		t.Error("OfflinePlan with a bad ID: want an error")
	}
}

func TestSlotOfVeth(t *testing.T) {
	for name, want := range map[string]int{"tine0": 0, "tine42": 42, "tine16383": 16383} {
		if got, ok := SlotOfVeth(name); !ok || got != want {
			t.Errorf("SlotOfVeth(%q) = %d, %v; want %d", name, got, ok, want)
		}
	}
	for _, bad := range []string{"tine", "tine-1", "tine16384", "tine007", "eth0", "tinex"} {
		if _, ok := SlotOfVeth(bad); ok {
			t.Errorf("SlotOfVeth(%q): want not ok", bad)
		}
	}
}
