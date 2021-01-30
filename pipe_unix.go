// +build aix darwin dragonfly freebsd linux netbsd openbsd solaris

package mux9p

import (
	"errors"

	"golang.org/x/sys/unix"
)

func pipe(p []int) error {
	if len(p) != 2 {
		return errors.New("bad argument to pipe")
	}
	fd, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		return err
	}
	copy(p, fd[:])
	return nil
}
