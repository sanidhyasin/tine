package image

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func write(t *testing.T, path, content string, perm os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), perm); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path) //nolint:gosec // test file
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func readlink(t *testing.T, path string) string {
	t.Helper()
	target, err := os.Readlink(path)
	if err != nil {
		t.Fatalf("%s isn't a symlink: %v", path, err)
	}
	return target
}

// fakeGuest makes a small root filesystem that looks enough like Ubuntu's.
func fakeGuest(t *testing.T) (root, hostFile string) {
	t.Helper()
	root = t.TempDir()
	write(t, filepath.Join(root, "etc/hostname"), "ubuntu-fc-uvm\n", 0o644)
	if err := os.MkdirAll(filepath.Join(root, "etc/systemd/system/multi-user.target.wants"), 0o750); err != nil {
		t.Fatal(err)
	}
	// SSH enabled, as in the real image.
	if err := os.Symlink("/lib/systemd/system/ssh.service", filepath.Join(root, "etc/systemd/system/multi-user.target.wants/ssh.service")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/lib/systemd/system/ssh.socket", filepath.Join(root, "etc/systemd/system/ssh.socket")); err != nil {
		t.Fatal(err)
	}
	// /etc/resolv.conf is an absolute symlink. Unpacked on the host, it points
	// at a host file: the test's sentinel, which must never change.
	hostFile = filepath.Join(t.TempDir(), "host-resolv.conf")
	write(t, hostFile, "the host's own file\n", 0o644)
	if err := os.Symlink(hostFile, filepath.Join(root, "etc/resolv.conf")); err != nil {
		t.Fatal(err)
	}
	return root, hostFile
}

func fakeAgent(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "tine-agent")
	write(t, p, "#!/bin/sh\necho tine-agent v9.9.9\n", 0o755)
	return p
}

func TestCustomize(t *testing.T) {
	root, hostFile := fakeGuest(t)
	agent := fakeAgent(t)
	meta := Metadata{Name: "base", CreatedAt: time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC), Agent: AgentInfo{Version: "v9.9.9"}}

	if err := Customize(root, agent, meta); err != nil {
		t.Fatalf("Customize: %v", err)
	}

	// The agent is installed and executable.
	installed := filepath.Join(root, AgentPath)
	if read(t, installed) != read(t, agent) {
		t.Error("installed agent differs from the source")
	}
	if info, _ := os.Stat(installed); info.Mode().Perm() != 0o755 {
		t.Errorf("agent mode = %v, want 0755", info.Mode().Perm())
	}
	// Its unit starts it early and keeps restarting it.
	unit := read(t, filepath.Join(root, AgentUnitPath))
	for _, want := range []string{"ExecStart=" + AgentPath, "Restart=always", "StartLimitIntervalSec=0", "DefaultDependencies=no", "WantedBy=sysinit.target"} {
		if !strings.Contains(unit, want) {
			t.Errorf("agent unit lacks %q", want)
		}
	}
	if got := readlink(t, filepath.Join(root, "etc/systemd/system/sysinit.target.wants/tine-agent.service")); got != AgentUnitPath {
		t.Errorf("agent is enabled via %q, want %q", got, AgentUnitPath)
	}
	// SSH and background updates are masked.
	for _, unit := range maskedUnits {
		if got := readlink(t, filepath.Join(root, "etc/systemd/system", unit)); got != "/dev/null" {
			t.Errorf("%s points to %q, want /dev/null (masked)", unit, got)
		}
	}
	// DNS is a real file now, and the host's file is untouched.
	resolvPath := filepath.Join(root, "etc/resolv.conf")
	if info, _ := os.Lstat(resolvPath); info.Mode()&os.ModeSymlink != 0 {
		t.Error("/etc/resolv.conf is still a symlink")
	}
	if resolv := read(t, resolvPath); !strings.Contains(resolv, "nameserver 1.1.1.1\nnameserver 8.8.8.8\n") {
		t.Errorf("resolv.conf = %q", resolv)
	}
	if got := read(t, hostFile); got != "the host's own file\n" {
		t.Fatalf("Customize wrote through the symlink to the host's file: %q", got)
	}
	if got := read(t, filepath.Join(root, "etc/hostname")); got != "tine\n" {
		t.Errorf("hostname = %q", got)
	}
	var info map[string]any
	if err := json.Unmarshal([]byte(read(t, filepath.Join(root, ImageInfoPath))), &info); err != nil || info["image"] != "base" || info["agent_version"] != "v9.9.9" {
		t.Errorf("%s = %v (%v)", ImageInfoPath, info, err)
	}

	// Stamps that stop every boot from rerunning ldconfig and friends.
	for _, stamp := range []string{"etc/.updated", "var/.updated"} {
		if _, err := os.Stat(filepath.Join(root, stamp)); err != nil {
			t.Errorf("missing %s: %v", stamp, err)
		}
	}

	// Running it again (as an import with Force would) works and changes nothing important.
	if err := Customize(root, agent, meta); err != nil {
		t.Errorf("second Customize: %v", err)
	}
}

func TestCustomizeRefusesSymlinkedDirectories(t *testing.T) {
	root, _ := fakeGuest(t)
	outside := t.TempDir()
	// The guest's /etc/systemd points outside the guest.
	if err := os.RemoveAll(filepath.Join(root, "etc/systemd")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "etc/systemd")); err != nil {
		t.Fatal(err)
	}
	if err := Customize(root, fakeAgent(t), Metadata{Name: "base"}); err == nil {
		t.Fatal("Customize through a symlinked directory: want an error")
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Errorf("Customize wrote %d entries outside the guest", len(entries))
	}
}

func TestImportValidatesBeforeDoingAnything(t *testing.T) {
	dir := t.TempDir()
	kernel := filepath.Join(dir, "vmlinux")
	sqfs := filepath.Join(dir, "rootfs.squashfs")
	write(t, kernel, "k", 0o644)
	write(t, sqfs, "s", 0o644)
	agent := fakeAgent(t)
	good := ImportOptions{Name: "base", ImagesDir: filepath.Join(dir, "images"), Kernel: kernel, RootfsSquashfs: sqfs, Agent: agent}

	for name, change := range map[string]func(*ImportOptions){
		"empty name":     func(o *ImportOptions) { o.Name = "" },
		"uppercase name": func(o *ImportOptions) { o.Name = "Base" },
		"path in name":   func(o *ImportOptions) { o.Name = "../base" },
		"long name":      func(o *ImportOptions) { o.Name = strings.Repeat("a", 33) },
		"no kernel":      func(o *ImportOptions) { o.Kernel = filepath.Join(dir, "nope") },
		"kernel is dir":  func(o *ImportOptions) { o.Kernel = dir },
		"no squashfs":    func(o *ImportOptions) { o.RootfsSquashfs = "" },
		"no agent":       func(o *ImportOptions) { o.Agent = filepath.Join(dir, "nope") },
		"tiny disk":      func(o *ImportOptions) { o.SizeBytes = 1 << 20 },
	} {
		t.Run(name, func(t *testing.T) {
			opts := good
			change(&opts)
			if _, err := Import(context.Background(), opts); err == nil {
				t.Error("Import: want an error")
			}
		})
	}
	if _, err := os.Stat(good.ImagesDir); !os.IsNotExist(err) {
		t.Error("a failed validation still created the images directory")
	}

	// An existing image needs Force.
	if err := os.MkdirAll(filepath.Join(good.ImagesDir, "base"), 0o750); err != nil {
		t.Fatal(err)
	}
	if _, err := Import(context.Background(), good); !errors.Is(err, ErrExists) {
		t.Errorf("Import over an existing image = %v, want ErrExists", err)
	}
}

// TestImport runs a whole import on a tiny squashfs where the tools exist
// (Linux with squashfs-tools and e2fsprogs). It skips elsewhere.
func TestImport(t *testing.T) {
	for _, tool := range []string{"mksquashfs", "unsquashfs", "mkfs.ext4", "e2fsck"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s isn't installed; the integration test covers imports", tool)
		}
	}
	dir := t.TempDir()
	tree, _ := fakeGuest(t)
	_ = os.Remove(filepath.Join(tree, "etc/resolv.conf")) // squashing a link to a host temp file isn't useful here
	sqfs := filepath.Join(dir, "ubuntu-24.04.squashfs")
	if out, err := exec.CommandContext(context.Background(), "mksquashfs", tree, sqfs, "-quiet", "-no-progress").CombinedOutput(); err != nil { //nolint:gosec // test tool
		t.Fatalf("mksquashfs: %v\n%s", err, out)
	}
	kernel := filepath.Join(dir, "vmlinux-6.18.56")
	write(t, kernel, "not really a kernel", 0o644)
	images := filepath.Join(dir, "images")

	opts := ImportOptions{Name: "base", ImagesDir: images, Kernel: kernel, RootfsSquashfs: sqfs, Agent: fakeAgent(t), SizeBytes: 256 << 20}
	img, err := Import(context.Background(), opts)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if img.Dir != filepath.Join(images, "base") || img.Meta.Kernel.Source != "vmlinux-6.18.56" || img.Meta.Rootfs.Source != "ubuntu-24.04.squashfs" {
		t.Errorf("image = %+v", img)
	}
	if img.Meta.Kernel.SHA256 == "" || img.Meta.Rootfs.SourceSHA256 == "" || img.Meta.Agent.SHA256 == "" {
		t.Error("metadata is missing a hash")
	}
	if img.Meta.Agent.Version != "v9.9.9" {
		t.Errorf("agent version = %q, want v9.9.9 (from running it)", img.Meta.Agent.Version)
	}
	if info, err := os.Stat(img.RootfsPath()); err != nil || info.Size() != 256<<20 {
		t.Errorf("disk image: %v, %v; want 256 MiB", info, err)
	}
	if info, _ := os.Stat(img.KernelPath()); info.Mode().Perm() != 0o644 {
		t.Errorf("kernel mode %v, want 0644 so jails can share it", info.Mode().Perm())
	}
	// No temporary directories are left in the images directory.
	entries, _ := os.ReadDir(images)
	if len(entries) != 1 {
		t.Errorf("images directory holds %d entries, want just the image", len(entries))
	}

	opened, err := Open(images, "base")
	if err != nil || opened.Meta.Rootfs.SourceSHA256 != img.Meta.Rootfs.SourceSHA256 {
		t.Errorf("Open = %+v, %v", opened, err)
	}

	// Replacing it needs Force, and then works.
	if _, err := Import(context.Background(), opts); !errors.Is(err, ErrExists) {
		t.Errorf("second Import = %v, want ErrExists", err)
	}
	opts.Force = true
	if _, err := Import(context.Background(), opts); err != nil {
		t.Errorf("Import with Force: %v", err)
	}
	if entries, _ := os.ReadDir(images); len(entries) != 1 {
		t.Errorf("after replacing, images directory holds %d entries, want 1", len(entries))
	}
}

func TestOpenAndList(t *testing.T) {
	images := t.TempDir()
	makeImage := func(name string, meta Metadata, files ...string) {
		dir := filepath.Join(images, name)
		data, _ := json.Marshal(meta)
		write(t, filepath.Join(dir, MetadataFile), string(data), 0o644)
		for _, f := range files {
			write(t, filepath.Join(dir, f), "x", 0o644)
		}
	}
	makeImage("base", Metadata{FormatVersion: MetadataVersion, Name: "base"}, KernelFile, RootfsFile)
	makeImage("alpha", Metadata{FormatVersion: MetadataVersion, Name: "alpha"}, KernelFile, RootfsFile)
	makeImage("future", Metadata{FormatVersion: MetadataVersion + 1}, KernelFile, RootfsFile)
	makeImage("broken", Metadata{FormatVersion: MetadataVersion}, KernelFile) // no disk
	makeImage(".import-x-123", Metadata{FormatVersion: MetadataVersion}, KernelFile, RootfsFile)

	list, err := List(images)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, img := range list {
		names = append(names, img.Name)
	}
	if strings.Join(names, ",") != "alpha,base" {
		t.Errorf("List = %v, want [alpha base]", names)
	}
	for _, name := range []string{"future", "broken", "nope", "../etc"} {
		if _, err := Open(images, name); err == nil {
			t.Errorf("Open(%q): want an error", name)
		}
	}
	if list, err := List(filepath.Join(images, "missing")); err != nil || len(list) != 0 {
		t.Errorf("List of a missing directory = %v, %v; want empty", list, err)
	}
}
