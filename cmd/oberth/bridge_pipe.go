package main

import (
	"io"
	"os"
	"syscall"
)

// Inherited pipes need pollable descriptors so cancellation can close blocked I/O.
func bridgePipe(stream any) (*os.File, error) {
	file, ok := stream.(*os.File)
	if !ok {
		return nil, nil
	}
	info, err := file.Stat()
	if err != nil || info.Mode()&os.ModeNamedPipe == 0 {
		return nil, err
	}
	fd, err := syscall.Dup(int(file.Fd()))
	if err != nil {
		return nil, err
	}
	syscall.CloseOnExec(fd)
	if err = syscall.SetNonblock(fd, true); err != nil {
		_ = syscall.Close(fd)
		return nil, err
	}
	return os.NewFile(uintptr(fd), file.Name()), nil
}

func (a *application) bridgePipes() (func(), error) {
	var opened []io.Closer
	closePipes := func() {
		for _, file := range opened {
			_ = file.Close()
		}
	}
	for i, stream := range []any{a.in, a.out} {
		pipe, err := bridgePipe(stream)
		if err != nil {
			closePipes()
			return nil, err
		}
		if pipe == nil {
			continue
		}
		opened = append(opened, pipe)
		if i == 0 {
			a.in = pipe
		} else {
			a.out = pipe
		}
	}
	return closePipes, nil
}
