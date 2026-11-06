//go:build !linux

package netns

import (
	"context"
	"errors"
)

// errUnsupported is returned everywhere but Linux: VM networks need Linux
// network namespaces.
var errUnsupported = errors.New("VM networks need Linux")

// Setup needs Linux.
func Setup(context.Context) error { return errUnsupported }

// RemoveHostRules needs Linux.
func RemoveHostRules(context.Context) error { return errUnsupported }

// Create needs Linux.
func Create(context.Context, Network, Owner) error { return errUnsupported }

// Destroy needs Linux.
func Destroy(Network) error { return errUnsupported }

// Leftovers needs Linux.
func Leftovers() ([]string, error) { return nil, errUnsupported }
