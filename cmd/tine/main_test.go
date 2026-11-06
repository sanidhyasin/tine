package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestVersionCommand(t *testing.T) {
	var out bytes.Buffer
	root := newRootCmd()
	root.SetOut(&out)
	root.SetArgs([]string{"version"})

	if err := root.Execute(); err != nil {
		t.Fatalf("tine version: %v", err)
	}
	if got := out.String(); !strings.HasPrefix(got, "tine dev ") {
		t.Errorf("tine version printed %q, want it to start with %q", got, "tine dev ")
	}
}

func TestVersionCommandRejectsArgs(t *testing.T) {
	root := newRootCmd()
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"version", "extra"})

	if err := root.Execute(); err == nil {
		t.Error("tine version extra: want an error, got none")
	}
}
