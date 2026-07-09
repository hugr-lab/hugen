//go:build !linux

package main

import "syscall"

// dupFd2 duplicates oldfd onto newfd. Non-Linux unixes (darwin / BSD) provide
// dup2 but not dup3.
func dupFd2(oldfd, newfd int) error {
	return syscall.Dup2(oldfd, newfd)
}
