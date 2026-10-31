//go:build unix

package control

import "syscall"

func setUmask(m int) int { return syscall.Umask(m) }
