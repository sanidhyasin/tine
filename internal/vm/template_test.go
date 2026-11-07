package vm

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sanidhyasin/tine/internal/image"
	"github.com/sanidhyasin/tine/internal/state"
)

func warmFixture(t *testing.T, change func(*Config)) *fixture {
	t.Helper()
	f := newFixture(t, change)
	f.host.agentClient = startAgent(t)
	return f
}

func TestWarmTemplateAndCreateFromIt(t *testing.T) {
	ctx := context.Background()
	f := warmFixture(t, nil)
	tmpl, err := f.m.WarmTemplate(ctx, "base", 0, 0)
	if err != nil {
		t.Fatalf("WarmTemplate: %v", err)
	}
	if tmpl.Kind != state.SnapshotKindTemplate || tmpl.Image != "base" || tmpl.VCPUs != 2 || tmpl.MemoryMiB != 1024 {
		t.Errorf("template = %+v", tmpl)
	}
	// The VM booted to make it is gone.
	if vm, _ := f.store.GetVM(ctx, tmpl.VMID); vm.State != state.StateDestroyed {
		t.Errorf("the template's VM is %s, want destroyed", vm.State)
	}
	_, _ = f.m.waitForFlush(ctx, tmpl.VMID)
	f.host.takeCalls()

	// A create of that image and size restores from it.
	vm, err := f.m.Create(ctx, CreateRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if vm.Source != state.SourceTemplate || vm.SnapshotID != tmpl.ID {
		t.Errorf("VM source %q, snapshot %q; want template %s", vm.Source, vm.SnapshotID, tmpl.ID)
	}
	if got := strings.Join(f.host.takeCalls(), ","); !strings.Contains(got, "loadSnapshot") || strings.Contains(got, "configureAndBoot") {
		t.Errorf("host calls = %s, want a restore", got)
	}
	// --cold, or another size, boots.
	for _, req := range []CreateRequest{{Cold: true}, {MemoryMiB: 512}} {
		vm, err := f.m.Create(ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		if vm.Source != state.SourceCold || vm.SnapshotID != "" {
			t.Errorf("Create(%+v): source %q, snapshot %q; want a cold boot", req, vm.Source, vm.SnapshotID)
		}
	}
	if _, err := f.m.Create(ctx, CreateRequest{Cold: true, SnapshotID: tmpl.ID}); !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("cold and a snapshot = %v, want ErrInvalidRequest", err)
	}
}

func TestUnusableTemplatesAreSkipped(t *testing.T) {
	ctx := context.Background()
	for _, tt := range []struct {
		name, want string
		spoil      func(f *fixture)
	}{
		{"another Firecracker", "incompatible", func(f *fixture) {
			other := fakeSnapshotHost
			other.Firecracker = "1.18.0"
			f.host.thisHost = &other
		}},
		{"the image imported again", "imported again", func(f *fixture) {
			reimport(t, f.m.cfg.ImagesDir, "base", time.Now())
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := warmFixture(t, nil)
			tmpl, err := f.m.WarmTemplate(ctx, "base", 0, 0)
			if err != nil {
				t.Fatal(err)
			}
			tt.spoil(f)
			vm, err := f.m.Create(ctx, CreateRequest{})
			if err != nil {
				t.Fatal(err)
			}
			if vm.Source != state.SourceCold {
				t.Errorf("source = %q, want cold", vm.Source)
			}
			events, _ := f.store.Events(ctx, vm.ID)
			found := false
			for _, e := range events {
				found = found || (strings.HasPrefix(e.Detail, "booted, not restored") && strings.Contains(e.Detail, tt.want))
			}
			if !found {
				t.Errorf("no note saying why it booted (%q) in %+v", tt.want, events)
			}
			templates, _ := f.m.Templates(ctx)
			if len(templates) != 1 || templates[0].Snapshot.ID != tmpl.ID || !strings.Contains(templates[0].Problem, tt.want) {
				t.Errorf("Templates = %+v", templates)
			}
		})
	}
}

func TestKeepWarm(t *testing.T) {
	ctx := context.Background()
	f := warmFixture(t, func(c *Config) { c.WarmImages = []string{"base", "not-installed"} })
	f.m.keepWarm(ctx)
	eventually(t, "a template of base", func() bool {
		templates, _ := f.m.Templates(ctx)
		return len(templates) == 1 && templates[0].Problem == ""
	})
	eventually(t, "the warm to finish", func() bool {
		f.m.mu.Lock()
		defer f.m.mu.Unlock()
		return !f.m.warming["base"]
	})
	// Usable: nothing more to do.
	f.host.takeCalls()
	f.m.keepWarm(ctx)
	time.Sleep(50 * time.Millisecond)
	if calls := f.host.takeCalls(); len(calls) != 0 {
		t.Errorf("keepWarm with a usable template did %v", calls)
	}
	// Out of date: it warms a new one, which replaces it.
	old, _ := f.m.Templates(ctx)
	reimport(t, f.m.cfg.ImagesDir, "base", time.Now())
	f.m.keepWarm(ctx)
	eventually(t, "a new template", func() bool {
		templates, _ := f.m.Templates(ctx)
		return len(templates) == 1 && templates[0].Snapshot.ID != old[0].Snapshot.ID && templates[0].Problem == ""
	})
}

func TestKeepWarmWaitsAfterAFailure(t *testing.T) {
	ctx := context.Background()
	f := warmFixture(t, func(c *Config) { c.WarmImages = []string{"base"} })
	f.host.fail("configureAndBoot", errors.New("no KVM"))
	f.m.keepWarm(ctx)
	eventually(t, "the warm to fail", func() bool {
		f.m.mu.Lock()
		defer f.m.mu.Unlock()
		return !f.m.warmFailed["base"].IsZero() && !f.m.warming["base"]
	})
	f.host.takeCalls()
	f.m.keepWarm(ctx)
	time.Sleep(50 * time.Millisecond)
	if calls := f.host.takeCalls(); len(calls) != 0 {
		t.Errorf("it tried again at once: %v", calls)
	}
}

// reimport rewrites an image's metadata as if it had been imported again at t.
func reimport(t *testing.T, imagesDir, name string, at time.Time) {
	t.Helper()
	meta, _ := json.Marshal(image.Metadata{FormatVersion: image.MetadataVersion, Name: name, CreatedAt: at})
	if err := os.WriteFile(filepath.Join(imagesDir, name, image.MetadataFile), meta, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestPrefetchTemplates(t *testing.T) {
	ctx := context.Background()
	f := warmFixture(t, nil)
	tmpl, err := f.m.WarmTemplate(ctx, "base", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	other := fakeSnapshotHost
	f.host.takeCalls()
	f.m.prefetchTemplates(ctx)
	if calls := strings.Join(f.host.takeCalls(), ","); calls != "prefetch "+tmpl.ID {
		t.Errorf("calls = %s, want the template prefetched", calls)
	}
	// An unusable template isn't worth reading.
	other.Kernel = "7.0"
	f.host.thisHost = &other
	f.m.prefetchTemplates(ctx)
	if calls := f.host.takeCalls(); len(calls) != 0 {
		t.Errorf("prefetched an unusable template: %v", calls)
	}
}
