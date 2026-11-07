//go:build !linux

package vm

import "errors"

func enableKSM() error { return errors.New("ksm needs Linux") }
