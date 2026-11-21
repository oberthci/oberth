package verifytest

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func parentArguments() ([]string, error) {
	raw, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(os.Getppid()), "cmdline"))
	if err != nil {
		return nil, err
	}
	return strings.Split(strings.TrimSuffix(string(raw), "\x00"), "\x00"), nil
}

func parentDirectory(context.Context) (string, error) {
	return os.Readlink(filepath.Join("/proc", strconv.Itoa(os.Getppid()), "cwd"))
}
