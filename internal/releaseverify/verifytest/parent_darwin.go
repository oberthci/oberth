package verifytest

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

func parentArguments() ([]string, error) {
	raw, err := unix.SysctlRaw("kern.procargs2", os.Getppid())
	if err != nil {
		return nil, err
	}
	if len(raw) < 4 {
		return nil, errors.New("parent process arguments are truncated")
	}
	count := int(binary.NativeEndian.Uint32(raw[:4]))
	_, raw, ok := bytes.Cut(raw[4:], []byte{0})
	if !ok {
		return nil, errors.New("parent executable path is missing")
	}
	parts := bytes.SplitN(bytes.TrimLeft(raw, "\x00"), []byte{0}, count+1)
	if count >= len(parts) {
		return nil, errors.New("parent process arguments are truncated")
	}
	args := make([]string, count)
	for i := range args {
		args[i] = string(parts[i])
	}
	return args, nil
}

func parentDirectory(ctx context.Context) (string, error) {
	command := exec.CommandContext(ctx, "/usr/sbin/lsof", "-a", "-p", strconv.Itoa(os.Getppid()), "-d", "cwd", "-F0n")
	command.WaitDelay = 5 * time.Second
	raw, err := command.Output()
	if err != nil {
		return "", err
	}
	for _, field := range strings.Split(string(raw), "\x00") {
		if path, ok := strings.CutPrefix(field, "n"); ok {
			return path, nil
		}
	}
	return "", errors.New("parent working directory is missing")
}
