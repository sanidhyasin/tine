//go:build !linux

package agent

import (
	"errors"
	"time"
)

// The agent only runs in Linux guests; elsewhere, /sync can only be tested
// with a fake System.

func setClock(time.Time) error { return errors.ErrUnsupported }
func addEntropy([]byte) error  { return errors.ErrUnsupported }
func sethostname(string) error { return errors.ErrUnsupported }
