//go:build !linux

package vm

import (
	"context"
	"errors"

	"github.com/sanidhyasin/tine/internal/jailer"
)

func (h *hostOS) startPageServer(context.Context, *jailer.Jail, pageServerSpec) (pageServer, error) {
	return nil, errors.New("page servers need Linux")
}
