package jailer

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// controllers are the cgroup v2 controllers that VM limits use.
var controllers = []string{"cpu", "memory", "pids"}

// EnsureCgroupParent creates CgroupParent and turns on the cpu, memory and
// pids controllers for its children, all the way down from the cgroup root.
// Run it once when tined starts, before any jail. It's safe to repeat.
func (c Config) EnsureCgroupParent() error {
	if c.CgroupRoot == "" {
		c.CgroupRoot = DefaultCgroupRoot
	}
	if err := c.validate(); err != nil {
		return err
	}
	dir := c.CgroupRoot
	for _, part := range strings.Split(c.CgroupParent, "/") {
		if err := enableControllers(dir); err != nil {
			return err
		}
		dir = filepath.Join(dir, part)
		if err := os.Mkdir(dir, 0o750); err != nil && !errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("create cgroup %s: %w", dir, err)
		}
	}
	return enableControllers(dir)
}

// enableControllers makes the controllers available to dir's child cgroups.
func enableControllers(dir string) error {
	available, err := readFields(filepath.Join(dir, "cgroup.controllers"))
	if err != nil {
		return fmt.Errorf("cgroup %s: %w (is cgroup v2 mounted there?)", dir, err)
	}
	enabled, err := readFields(filepath.Join(dir, "cgroup.subtree_control"))
	if err != nil {
		return fmt.Errorf("cgroup %s: %w", dir, err)
	}
	var missing []string
	for _, c := range controllers {
		if !available[c] {
			return fmt.Errorf("cgroup %s: the %s controller isn't available", dir, c)
		}
		if !enabled[c] {
			missing = append(missing, "+"+c)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	if err := os.WriteFile(filepath.Join(dir, "cgroup.subtree_control"), []byte(strings.Join(missing, " ")), 0o644); err != nil { //nolint:gosec // cgroup control files have fixed permissions
		return fmt.Errorf("cgroup %s: enable %s: %w", dir, strings.Join(missing, " "), err)
	}
	return nil
}

func readFields(path string) (map[string]bool, error) {
	data, err := os.ReadFile(path) //nolint:gosec // path is built from tined's config
	if err != nil {
		return nil, err
	}
	fields := map[string]bool{}
	for _, f := range strings.Fields(string(data)) {
		fields[f] = true
	}
	return fields, nil
}

// removeCgroup removes a VM's cgroup. Right after a process exits, the kernel
// can briefly still count it, so EBUSY is retried for up to a second.
func removeCgroup(dir string) error {
	deadline := time.Now().Add(time.Second)
	for {
		err := os.Remove(dir)
		switch {
		case err == nil, errors.Is(err, fs.ErrNotExist):
			return nil
		case errors.Is(err, syscall.EBUSY) && time.Now().Before(deadline):
			time.Sleep(20 * time.Millisecond)
		default:
			return fmt.Errorf("remove cgroup %s: %w", dir, err)
		}
	}
}
