package api

import (
	"context"
	"os"
	"reflect"
	"testing"
)

func TestSSHCommand(t *testing.T) {
	t.Setenv("TINE_SSH", "")
	for target, want := range map[string][]string{
		"ssh://tine-dev":                    {"ssh", "-T", "-o", "BatchMode=yes", "--", "tine-dev", "tine", "dial-stdio"},
		"ssh://me@10.0.0.5:2222":            {"ssh", "-T", "-o", "BatchMode=yes", "-p", "2222", "--", "me@10.0.0.5", "tine", "dial-stdio"},
		"ssh://me@host/var/run/tine/x.sock": {"ssh", "-T", "-o", "BatchMode=yes", "--", "me@host", "tine", "dial-stdio", "--socket", "/var/run/tine/x.sock"},
	} {
		got, err := sshCommand(target)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("sshCommand(%q) = %q, %v; want %q", target, got, err, want)
		}
	}
	t.Setenv("TINE_SSH", "ssh -F /tmp/cfg")
	if got, _ := sshCommand("ssh://h"); !reflect.DeepEqual(got[:3], []string{"ssh", "-F", "/tmp/cfg"}) {
		t.Errorf("with TINE_SSH: %q", got)
	}
	for _, bad := range []string{"tine-dev", "http://tine-dev", "ssh://", "ssh:///path"} {
		if _, err := sshCommand(bad); err == nil {
			t.Errorf("sshCommand(%q) succeeded", bad)
		}
	}
}

// The far end of TestCommandClient: this test binary, run again, as
// "ssh host tine dial-stdio" would run tine.
func TestHelperDialStdio(t *testing.T) {
	sock := os.Getenv("TINE_TEST_DIAL_STDIO")
	if sock == "" {
		t.Skip("only run by TestCommandClient")
	}
	if err := DialStdio(context.Background(), sock, os.Stdin, os.Stdout); err != nil {
		os.Exit(3)
	}
	os.Exit(0)
}

func TestCommandClient(t *testing.T) {
	_, sock := serve(t, newFakeBackend())
	t.Setenv("TINE_TEST_DIAL_STDIO", sock)
	c := NewCommandClient([]string{os.Args[0], "-test.run=^TestHelperDialStdio$"})
	ctx := context.Background()
	h, err := c.Health(ctx)
	if err != nil || h.Status != "ok" {
		t.Fatalf("Health through a command: %+v, %v", h, err)
	}
	// Several calls, one after another and at once: each connection is a command.
	v, err := c.CreateVM(ctx, CreateVMRequest{})
	if err != nil {
		t.Fatal(err)
	}
	errs := make(chan error, 5)
	for range 5 {
		go func() { _, err := c.GetVM(ctx, v.ID); errs <- err }()
	}
	for range 5 {
		if err := <-errs; err != nil {
			t.Errorf("GetVM through a command: %v", err)
		}
	}
	if _, err := NewSSHClient("not-a-url"); err == nil {
		t.Error("a bad host was accepted")
	}
}
