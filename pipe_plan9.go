package mux9p

import "syscall"

func pipe(p []int) error {
	return syscall.Pipe(p)
}
