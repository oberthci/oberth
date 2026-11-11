package installer

import (
	"encoding/json"
	"errors"
	"io"
	"os"

	"sigs.k8s.io/yaml"
)

// Answers is the versioned, non-secret input shared by the CLI and setup wizard.
// Config field names are the Go engine's documented names; credentials belong
// in files named by the configuration, never in this document.
type Answers struct {
	Version int    `json:"version"`
	Config  Config `json:"config"`
}

func ReadAnswers(path string) (Config, error) {
	file, err := os.Open(path) //nolint:gosec // G304: explicitly selected answers file
	if err != nil {
		return Config{}, errors.New("cannot open answers file")
	}
	defer func() { _ = file.Close() }()
	raw, err := io.ReadAll(io.LimitReader(file, (1<<20)+1))
	defer clear(raw)
	if err != nil || len(raw) > 1<<20 {
		return Config{}, errors.New("cannot read answers file (maximum 1 MiB)")
	}
	var answers Answers
	if err := yaml.UnmarshalStrict(raw, &answers); err != nil {
		return Config{}, errors.New("invalid answers file: use the documented version/config schema; unknown or duplicate fields are refused")
	}
	if answers.Version != 1 {
		return Config{}, errors.New("answers version must be 1")
	}
	return answers.Config, nil
}

func WriteAnswers(path string, cfg Config) error {
	cfg.BinaryVersion = ""
	raw, err := json.MarshalIndent(Answers{Version: 1, Config: cfg}, "", "  ")
	if err != nil {
		return errors.New("cannot encode install answers")
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // G304: operator-selected new file; O_EXCL refuses existing paths and symlinks
	if err != nil {
		return errors.New("cannot create answers file: choose a new writable path")
	}
	defer func() { _ = file.Close() }()
	if err := file.Chmod(0o600); err != nil {
		return err
	}
	if _, err := file.Write(append(raw, '\n')); err != nil {
		return err
	}
	return file.Sync()
}
