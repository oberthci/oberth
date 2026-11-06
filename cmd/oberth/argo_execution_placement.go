package main

import (
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/oberthci/oberth/internal/argojob"
)

func readArgoExecutionPlacement(name string) (*argojob.ExecutionPlacement, error) {
	if name == "" {
		return nil, nil
	}
	if !filepath.IsAbs(name) || filepath.Clean(name) != name {
		return nil, errors.New("serve: execution placement requires a clean absolute path")
	}
	// #nosec G304 -- administrator flag; never a request or repository path.
	file, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	body, err := io.ReadAll(io.LimitReader(file, 65537))
	if err != nil {
		return nil, err
	}
	return argojob.ParseExecutionPlacement(body)
}
