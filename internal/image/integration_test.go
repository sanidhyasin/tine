//go:build integration

package image

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/sanidhyasin/tine/internal/agentproto"
	"github.com/sanidhyasin/tine/internal/firecracker"
	"github.com/sanidhyasin/tine/internal/jailer"
	"github.com/sanidhyasin/tine/internal/netns"
	"github.com/sanidhyasin/tine/internal/testenv"
)

// TestImportAndBoot builds a real image from Firecracker's Ubuntu, boots a
// VM from it in a jail on its own network, and talks to tine-agent over real
// vsock: it's ready on its own, runs commands, has working DNS and internet,
// and no SSH server.
func TestImportAndBoot(t *testing.T) {
	testenv.RequireRoot(t)
	testenv.RequireKVM(t)
	a := testenv.RequireArtifacts(t)
	agentBin := testenv.RequireAgentBinary(t)
	data := testenv.RequireDataDir(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	// --- Import ---
	images := testenv.TempDir(t, data)
	img, err := Import(ctx, ImportOptions{Name: "base", ImagesDir: images, Kernel: a.Kernel, RootfsSquashfs: a.Squashfs, Agent: agentBin})
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	t.Logf("imported in %.1fs: %s from %s, agent %q", img.Meta.BuildSeconds, img.Meta.Kernel.Source, img.Meta.Rootfs.Source, img.Meta.Agent.Version)
	if img.Meta.Agent.Version == "" {
		t.Error("the agent's version wasn't recorded")
	}
	if list, err := List(images); err != nil || len(list) != 1 || list[0].Name != "base" {
		t.Errorf("List = %v, %v; want just base", list, err)
	}

	// --- Boot ---
	if err := netns.Setup(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = netns.RemoveHostRules(context.Background()) })
	const uid = 299_970
	id := "itest-img-" + randomHex(t)
	n, err := netns.Plan(id, 300)
	if err != nil {
		t.Fatal(err)
	}
	if err := netns.Create(ctx, n, netns.Owner{UID: uid, GID: uid}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := netns.Destroy(n); err != nil {
			t.Errorf("destroy network: %v", err)
		}
	})
	j, bootStart := boot(t, ctx, a, img, data, n, uid)

	// --- The agent, over real vsock ---
	agent := agentproto.NewVsockClient(j.HostPath("/vsock.sock"))
	health, err := agent.WaitHealthy(ctx, 10*time.Millisecond)
	if err != nil {
		t.Fatalf("the agent never answered: %v", err)
	}
	bootToReady := time.Since(bootStart)
	if health.Protocol != agentproto.Version {
		t.Errorf("agent speaks protocol %d, want %d", health.Protocol, agentproto.Version)
	}

	exec := func(args ...string) (string, agentproto.ExitStatus) {
		t.Helper()
		var out strings.Builder
		st, err := agent.Exec(ctx, agentproto.ExecRequest{Args: args, TimeoutMs: 20_000}, func(_ string, d []byte) { out.Write(d) })
		if err != nil {
			t.Fatalf("exec %q: %v", args, err)
		}
		return out.String(), st
	}
	checks := []struct {
		what  string
		args  []string
		want  string // the output must contain this
		code0 bool   // and the command must succeed
	}{
		{"runs as root", []string{"id", "-u"}, "0", true},
		{"starts in /root", []string{"pwd"}, "/root", true},
		{"knows its image", []string{"cat", ImageInfoPath}, `"image": "base"`, true},
		{"has the guest kernel", []string{"uname", "-r"}, strings.TrimPrefix(img.Meta.Kernel.Source, "vmlinux-"), true},
		{"has static DNS", []string{"cat", "/etc/resolv.conf"}, "nameserver 1.1.1.1", true},
		{"resolves names", []string{"getent", "hosts", "example.com"}, "example.com", true},
		{"reaches the internet", []string{"timeout", "5", "bash", "-c", "echo > /dev/tcp/1.1.1.1/443"}, "", true},
		{"has the agent running under systemd", []string{"systemctl", "is-active", "tine-agent.service"}, "active", true},
	}
	for _, c := range checks {
		out, st := exec(c.args...)
		if (c.code0 && st.Code != 0) || !strings.Contains(out, c.want) {
			t.Errorf("%s: %q exited %d with %q; want code 0 and output containing %q", c.what, c.args, st.Code, out, c.want)
		}
	}
	if out, st := exec("systemctl", "is-active", "ssh.service", "ssh.socket"); st.Code == 0 || strings.Contains(out, "\nactive") || strings.HasPrefix(out, "active") {
		t.Errorf("SSH is running in the guest: %q", out)
	}

	// How long does a trivial command take, end to end?
	var rtts []time.Duration
	for i := 0; i < 20; i++ {
		start := time.Now()
		if _, st := exec("true"); st.Code != 0 {
			t.Fatalf("true exited %d", st.Code)
		}
		rtts = append(rtts, time.Since(start))
	}
	sort.Slice(rtts, func(i, j int) bool { return rtts[i] < rtts[j] })

	// Where boot time goes, as the guest saw it.
	if out, st := exec("systemd-analyze"); st.Code == 0 {
		t.Logf("systemd-analyze: %s", strings.TrimSpace(strings.Split(out, "\n")[0]))
	}
	if out, st := exec("systemctl", "show", "tine-agent.service", "-p", "ExecMainStartTimestampMonotonic", "--value"); st.Code == 0 {
		if us, err := time.ParseDuration(strings.TrimSpace(out) + "us"); err == nil {
			t.Logf("tine-agent started %v after the guest kernel", us.Round(time.Millisecond))
		}
	}
	if out, st := exec("sh", "-c", "systemd-analyze blame | head -5"); st.Code == 0 {
		t.Logf("slowest units:\n%s", out)
	}
	t.Logf("boot -> agent ready over vsock: %v", bootToReady.Round(time.Millisecond))
	t.Logf("exec round trip (true): median %v, slowest %v", rtts[len(rtts)/2].Round(time.Microsecond), rtts[len(rtts)-1].Round(time.Microsecond))
}

// boot starts a jailed VM from img on network n, and stops it when the test
// ends. It returns the jail and when the VM was started.
func boot(t *testing.T, ctx context.Context, a testenv.Artifacts, img *Image, data string, n netns.Network, uid int) (*jailer.Jail, time.Time) {
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
		Limits: jailer.Limits{VCPUs: 2, MemoryBytes: 1 << 30, MaxPIDs: 128},
		Files: []jailer.File{
			{Source: img.KernelPath(), Target: "/vmlinux", Mode: jailer.Link},
			{Source: img.RootfsPath(), Target: "/rootfs.ext4", Mode: jailer.Clone},
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
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		_ = console.Close()
		if t.Failed() {
			if out, err := os.ReadFile(j.ConsoleLogPath()); err == nil {
				s := string(out)
				if len(s) > 4000 {
					s = s[len(s)-4000:]
				}
				t.Logf("console:\n%s", s)
			}
		}
		_ = j.Cleanup()
	})

	c := firecracker.New(j.APISocket())
	steps := []func() error{
		func() error { return c.WaitReady(ctx, 2*time.Millisecond) },
		func() error {
			return c.PutMachineConfig(ctx, firecracker.MachineConfig{VCPUCount: 2, MemSizeMiB: 512})
		},
		func() error {
			return c.PutBootSource(ctx, firecracker.BootSource{
				KernelImagePath: "/vmlinux",
				BootArgs:        "console=ttyS0 reboot=k panic=1 " + netns.GuestKernelArgs(),
			})
		},
		func() error {
			return c.PutDrive(ctx, firecracker.Drive{DriveID: "rootfs", PathOnHost: "/rootfs.ext4", IsRootDevice: true})
		},
		func() error {
			return c.PutNetworkInterface(ctx, firecracker.NetworkInterface{IfaceID: "eth0", HostDevName: netns.TapDevice, GuestMAC: netns.GuestMAC})
		},
		func() error { return c.PutVsock(ctx, firecracker.Vsock{GuestCID: 3, UDSPath: "/vsock.sock"}) },
		func() error { return c.PutEntropy(ctx, firecracker.EntropyDevice{}) },
	}
	for i, step := range steps {
		if err := step(); err != nil {
			t.Fatalf("configure step %d: %v", i, err)
		}
	}
	start := time.Now()
	if err := c.StartInstance(ctx); err != nil {
		t.Fatal(err)
	}
	return j, start
}

func randomHex(t *testing.T) string {
	b := make([]byte, 3)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}
