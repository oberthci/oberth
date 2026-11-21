package gitssh

import (
	"io"
	"os"

	"golang.org/x/sys/unix"
)

// Native Git supplies a blocking pipe. Register a nonblocking duplicate with
// the runtime poller so closing it interrupts the reader when SSH ends.
func pollableInput(input io.ReadCloser) (io.ReadCloser, error) {
	file, ok := input.(*os.File)
	if !ok {
		return input, nil
	}
	fd, err := unix.Dup(int(file.Fd()))
	if err != nil {
		return nil, err
	}
	if err = unix.SetNonblock(fd, true); err != nil {
		_ = unix.Close(fd)
		return nil, err
	}
	return &ownedInput{File: os.NewFile(uintptr(fd), file.Name()), original: file}, nil
}

type ownedInput struct {
	*os.File
	original *os.File
}

func (f *ownedInput) Close() error {
	err := f.File.Close()
	_ = f.original.Close()
	return err
}
