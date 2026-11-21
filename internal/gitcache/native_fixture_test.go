package gitcache_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
)

//nolint:gochecknoglobals // Fault injection processes load independent copies of one compiled library.
var nativeFaultImage = sync.OnceValues(func() ([]byte, error) {
	directory, err := os.MkdirTemp(os.Getenv("GOTMPDIR"), "oberth-native-test-")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(directory) }()
	library := filepath.Join(directory, "fault.so")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, "/usr/bin/cc", "-shared", "-fPIC", "-O2", "-o", library, "testdata/fault.c", "-ldl")
	if raw, err := command.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("compile native kill fixture: %w: %s", err, raw)
	}
	return os.ReadFile(library)
})
