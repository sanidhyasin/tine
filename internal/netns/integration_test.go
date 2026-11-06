//go:build integration

package netns

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/vishvananda/netlink"
	vnetns "github.com/vishvananda/netns"

	"github.com/sanidhyasin/tine/internal/firecracker"
	"github.com/sanidhyasin/tine/internal/jailer"
	"github.com/sanidhyasin/tine/internal/testenv"
)

// TestManyNetworksLeaveNothingBehind creates 20 VM networks, checks each one,
// destroys them all, and checks nothing is left: the leak test from task 1.5.
func TestManyNetworksLeaveNothingBehind(t *testing.T) {
	testenv.RequireRoot(t)
	ctx := setupHost(t)
	requireNoLeftovers(t, "before the test")

	const count = 20
	run := randomHex(t)
	nets := make([]Network, count)
	var created, destroyed []time.Duration
	for i := range nets {
		n, err := Plan(fmt.Sprintf("itest-%s-%d", run, i), 100+i)
		if err != nil {
			t.Fatal(err)
		}
		nets[i] = n
		start := time.Now()
		if err := Create(ctx, n, Owner{UID: 299_900 + i, GID: 299_900 + i}); err != nil {
			t.Fatalf("Create %s: %v", n.ID, err)
		}
		created = append(created, time.Since(start))
		t.Cleanup(func() { _ = Destroy(n) })
	}

	for i, n := range nets {
		uid := 299_900 + i
		// The tap device exists inside the namespace, belongs to the VM's uid
		// and is set up. Its state stays DOWN ("NO-CARRIER") until Firecracker
		// attaches, so check the UP flag rather than the state.
		tap := run1(t, "ip", "-n", n.Namespace, "-d", "link", "show", TapDevice)
		for _, want := range []string{"tun type tap", "user " + strconv.Itoa(uid), "group " + strconv.Itoa(uid)} {
			if !strings.Contains(tap, want) {
				t.Errorf("%s: tap0 details lack %q:\n%s", n.Namespace, want, tap)
			}
		}
		if !tapFlagsUp.MatchString(tap) {
			t.Errorf("%s: tap0 isn't set up:\n%s", n.Namespace, tap)
		}
		if route := run1(t, "ip", "-n", n.Namespace, "route", "show", "default"); !strings.Contains(route, "via "+n.HostIP.String()) {
			t.Errorf("%s: default route %q doesn't go via %s", n.Namespace, route, n.HostIP)
		}
		if fw := run1(t, "ip", "netns", "exec", n.Namespace, "nft", "list", "table", "inet", "tine"); !strings.Contains(fw, "masquerade") {
			t.Errorf("%s: no NAT rule in the namespace's firewall:\n%s", n.Namespace, fw)
		}
		if fwd := strings.TrimSpace(run1(t, "ip", "netns", "exec", n.Namespace, "sysctl", "-n", "net.ipv4.ip_forward")); fwd != "1" {
			t.Errorf("%s: forwarding is %q, want 1", n.Namespace, fwd)
		}
		// The host reaches the namespace's end of the veth pair.
		if out, err := exec.CommandContext(ctx, "ping", "-c", "1", "-W", "1", n.NamespaceIP.String()).CombinedOutput(); err != nil { //nolint:gosec // test command
			t.Errorf("host can't ping %s in %s: %v\n%s", n.NamespaceIP, n.Namespace, err, out)
		}
	}

	for _, n := range nets {
		start := time.Now()
		if err := Destroy(n); err != nil {
			t.Errorf("Destroy %s: %v", n.ID, err)
		}
		destroyed = append(destroyed, time.Since(start))
		if err := Destroy(n); err != nil {
			t.Errorf("second Destroy %s: %v, want nil", n.ID, err)
		}
	}
	requireNoLeftovers(t, "after destroying every network")

	t.Logf("create:  median %v, slowest %v (%d networks)", median(created), slowest(created), count)
	t.Logf("destroy: median %v, slowest %v", median(destroyed), slowest(destroyed))
}

// TestVMNetworkIsolation boots a real VM on its own network and probes from
// inside the guest: the internet must work, and the metadata service, the
// host and another VM's network must not. Each blocked target is also
// reached from the host, which proves it was really listening, so the
// guest's failure is the firewall at work.
func TestVMNetworkIsolation(t *testing.T) {
	testenv.RequireRoot(t)
	testenv.RequireKVM(t)
	a := testenv.RequireArtifacts(t)
	data := testenv.RequireDataDir(t)
	ctx := setupHost(t)
	requireNoLeftovers(t, "before the test")

	run := randomHex(t)
	const uid = 299_980
	netA := createNetwork(t, ctx, "itest-"+run+"-a", 200, uid)
	netB := createNetwork(t, ctx, "itest-"+run+"-b", 201, uid+1)

	// Something listening in VM B's namespace, at B's veth address.
	nsB, err := vnetns.GetFromName(netB.Namespace)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = nsB.Close() }()
	var ln net.Listener
	if err := inNamespace(nsB, func() error {
		var lc net.ListenConfig
		var err error
		ln, err = lc.Listen(ctx, "tcp", netip.AddrPortFrom(netB.NamespaceIP, 9999).String())
		return err
	}); err != nil {
		t.Fatalf("listen in %s: %v", netB.Namespace, err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()

	bootVM(t, ctx, a, data, netA, uid)
	guest := func(cmd string) error {
		return exec.CommandContext(ctx, "ip", "netns", "exec", netA.Namespace, //nolint:gosec // test command
			"ssh", "-n", "-i", a.GuestKey, "-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null",
			"-o", "ConnectTimeout=2", "-o", "LogLevel=ERROR", "root@"+GuestIP.String(), cmd).Run()
	}
	deadline := time.Now().Add(90 * time.Second)
	for guest("true") != nil {
		if time.Now().After(deadline) {
			t.Fatal("the guest never answered SSH")
		}
		time.Sleep(500 * time.Millisecond)
	}
	tcpFromGuest := func(addr netip.AddrPort) bool {
		return guest(fmt.Sprintf("timeout 3 bash -c 'echo > /dev/tcp/%s/%d'", addr.Addr(), addr.Port())) == nil
	}
	tcpFromHost := func(addr netip.AddrPort) bool {
		d := net.Dialer{Timeout: 3 * time.Second}
		c, err := d.DialContext(ctx, "tcp", addr.String())
		if err == nil {
			_ = c.Close()
		}
		return err == nil
	}

	hostIP := mainHostIP(t)
	internet := netip.MustParseAddrPort("1.1.1.1:443")
	blocked := []struct {
		what   string
		target netip.AddrPort
		// hostCanReach: whether the host itself can connect, proving the target is live.
		hostCanReach bool
	}{
		{"the EC2 metadata service", netip.MustParseAddrPort("169.254.169.254:80"), true},
		{"the host's SSH, through the VM's own gateway", netip.AddrPortFrom(netA.HostIP, 22), true},
		{"the host's SSH, on its main address " + hostIP.String(), netip.AddrPortFrom(hostIP, 22), true},
		{"a listener in another VM's network", netip.AddrPortFrom(netB.NamespaceIP, 9999), true},
	}

	if !tcpFromGuest(internet) {
		t.Errorf("the guest can't reach the internet (%s)", internet)
	}
	for _, b := range blocked {
		if b.hostCanReach && !tcpFromHost(b.target) {
			t.Errorf("the host can't reach %s (%s) either, so this check proves nothing", b.what, b.target)
		}
		if tcpFromGuest(b.target) {
			t.Errorf("the guest reached %s (%s); the firewall should block it", b.what, b.target)
		} else {
			t.Logf("blocked as expected: %s (%s)", b.what, b.target)
		}
	}
}

// --- helpers ---

// tapFlagsUp matches an UP flag in `ip link` output, e.g. <NO-CARRIER,BROADCAST,MULTICAST,UP>.
var tapFlagsUp = regexp.MustCompile(`<[^>]*\bUP\b[^>]*>`)

func setupHost(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	if err := Setup(ctx); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	if err := Setup(ctx); err != nil {
		t.Fatalf("Setup a second time: %v", err)
	}
	t.Cleanup(func() { _ = RemoveHostRules(context.Background()) })
	return ctx
}

func createNetwork(t *testing.T, ctx context.Context, id string, slot, uid int) Network {
	t.Helper()
	n, err := Plan(id, slot)
	if err != nil {
		t.Fatal(err)
	}
	if err := Create(ctx, n, Owner{UID: uid, GID: uid}); err != nil {
		t.Fatalf("Create %s: %v", id, err)
	}
	t.Cleanup(func() {
		if err := Destroy(n); err != nil {
			t.Errorf("Destroy %s: %v", id, err)
		}
	})
	return n
}

// bootVM starts a jailed Firecracker VM on network n and stops it when the test ends.
func bootVM(t *testing.T, ctx context.Context, a testenv.Artifacts, data string, n Network, uid int) {
	t.Helper()
	cfg := jailer.Config{
		JailerBin:      a.Jailer,
		FirecrackerBin: a.Firecracker,
		ChrootBaseDir:  testenv.TempDir(t, data),
		CgroupParent:   "tine-test",
	}
	if err := cfg.EnsureCgroupParent(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove("/sys/fs/cgroup/" + cfg.CgroupParent) })

	j, err := jailer.New(cfg, jailer.Spec{
		ID:     n.ID,
		UID:    uid,
		GID:    uid,
		NetNS:  n.NamespacePath(),
		Limits: jailer.Limits{VCPUs: 1, MemoryBytes: 512 << 20, MaxPIDs: 64},
		Files: []jailer.File{
			{Source: testenv.CachedCopy(t, data, a.Kernel), Target: "/vmlinux", Mode: jailer.Link},
			{Source: testenv.CachedCopy(t, data, a.Rootfs), Target: "/rootfs.ext4", Mode: jailer.Clone},
		},
		FirecrackerArgs: []string{"--enable-pci"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := j.Prepare(); err != nil {
		t.Fatal(err)
	}
	console, err := os.Create(j.ConsoleLogPath())
	if err != nil {
		t.Fatal(err)
	}
	cmd := j.Command()
	cmd.Stdout, cmd.Stderr = console, console
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	// Registered after the network's cleanup, so it runs first: the VM must
	// be gone before its network can be.
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		_ = console.Close()
		if t.Failed() {
			if out, err := os.ReadFile(j.ConsoleLogPath()); err == nil {
				t.Logf("console:\n%s", tailString(string(out), 3000))
			}
		}
		_ = j.Cleanup()
	})

	c := firecracker.New(j.APISocket())
	steps := []func() error{
		func() error { return c.WaitReady(ctx, 2*time.Millisecond) },
		func() error {
			return c.PutMachineConfig(ctx, firecracker.MachineConfig{VCPUCount: 1, MemSizeMiB: 256})
		},
		func() error {
			return c.PutBootSource(ctx, firecracker.BootSource{
				KernelImagePath: "/vmlinux",
				BootArgs:        "console=ttyS0 reboot=k panic=1 " + GuestKernelArgs(),
			})
		},
		func() error {
			return c.PutDrive(ctx, firecracker.Drive{DriveID: "rootfs", PathOnHost: "/rootfs.ext4", IsRootDevice: true})
		},
		func() error {
			return c.PutNetworkInterface(ctx, firecracker.NetworkInterface{IfaceID: "eth0", HostDevName: TapDevice, GuestMAC: GuestMAC})
		},
		func() error { return c.StartInstance(ctx) },
	}
	for i, step := range steps {
		if err := step(); err != nil {
			t.Fatalf("boot step %d: %v", i, err)
		}
	}
}

// mainHostIP is the address the host uses to reach the internet.
func mainHostIP(t *testing.T) netip.Addr {
	t.Helper()
	routes, err := netlink.RouteGet(net.IPv4(1, 1, 1, 1))
	if err != nil || len(routes) == 0 {
		t.Fatalf("find the host's main address: %v", err)
	}
	addr, ok := netip.AddrFromSlice(routes[0].Src.To4())
	if !ok {
		t.Fatalf("bad source address %v", routes[0].Src)
	}
	return addr
}

func requireNoLeftovers(t *testing.T, when string) {
	t.Helper()
	left, err := Leftovers()
	if err != nil {
		t.Fatal(err)
	}
	if len(left) > 0 {
		t.Fatalf("VM networks exist %s: %v", when, left)
	}
}

func run1(t *testing.T, name string, args ...string) string {
	t.Helper()
	out, err := exec.CommandContext(context.Background(), name, args...).CombinedOutput() //nolint:gosec // test command
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
	return string(out)
}

func randomHex(t *testing.T) string {
	b := make([]byte, 3)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

func median(ds []time.Duration) time.Duration {
	s := append([]time.Duration(nil), ds...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	return s[len(s)/2].Round(time.Microsecond)
}

func slowest(ds []time.Duration) time.Duration {
	m := ds[0]
	for _, d := range ds {
		if d > m {
			m = d
		}
	}
	return m.Round(time.Microsecond)
}

func tailString(s string, n int) string {
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}
