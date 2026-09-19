//go:build !linux

package launcher

import (
	"context"
	"syscall"
)

func processAttributes() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setpgid: true} }

// reapOrphans is Linux-only; the launcher never runs as PID 1 elsewhere.
func reapOrphans(context.Context, int) {}
