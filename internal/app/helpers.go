package app

import (
	"fmt"
	"io"
	"os"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/pkg/periapsis"
)

func runTrigger(run model.Run) (periapsis.Trigger, error) {
	if run.Release {
		return periapsis.TriggerRelease, nil
	}
	return periapsis.TriggerCI, nil
}

// readBoundedSourceFile reads one bounded regular file from the immutable run
// workspace without escaping the source root.
func readBoundedSourceFile(sourceDir, relative string) ([]byte, error) {
	root, err := os.OpenRoot(sourceDir)
	if err != nil {
		return nil, fmt.Errorf("open immutable source root: %w", err)
	}
	defer func() { _ = root.Close() }()
	file, err := root.Open(relative)
	if err != nil {
		return nil, fmt.Errorf("open %s without escaping source root: %w", relative, err)
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", relative, err)
	}
	if !info.Mode().IsRegular() || info.Size() > periapsis.MaxSourceBytes {
		return nil, fmt.Errorf("%s must be a bounded regular file", relative)
	}
	source, err := io.ReadAll(io.LimitReader(file, periapsis.MaxSourceBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", relative, err)
	}
	if len(source) > periapsis.MaxSourceBytes {
		return nil, fmt.Errorf("%s exceeds the source-size limit", relative)
	}
	return source, nil
}
