//go:build !linux

package main

import (
	"errors"
	"time"
)

const defaultTimeout = 30 * time.Second

func run(bool, int, time.Duration) error { return errors.New("userfaultfd is Linux only") }
