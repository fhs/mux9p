package mux9p

import (
	"os"
)

// Pipe returns a two-way pipe. Unlike os.Pipe, read and write is supported on both ends.
func Pipe() (*os.File, *os.File, error) {
	var p [2]int
	if err := pipe(p[:]); err != nil {
		return nil, nil, err
	}
	return os.NewFile(uintptr(p[0]), "|0"), os.NewFile(uintptr(p[1]), "|1"), nil
}
