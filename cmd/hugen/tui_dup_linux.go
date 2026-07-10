//go:build linux

package main

import "syscall"

// dupFd2 duplicates oldfd onto newfd for the TUI stderr redirect. On Linux,
// syscall.Dup2 is absent on arm64/riscv64 (the kernel exposes only dup3),
// while Dup3 exists on every Linux arch — so we use it uniformly. flags=0
// makes it equivalent to dup2; the call sites never pass oldfd==newfd, which
// is dup3's one EINVAL case.
func dupFd2(oldfd, newfd int) error {
	return syscall.Dup3(oldfd, newfd, 0)
}
