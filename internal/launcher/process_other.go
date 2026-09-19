//go:build !linux

package launcher

import "syscall"

func processAttributes() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setpgid: true} }
