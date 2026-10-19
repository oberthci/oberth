package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/oberthci/oberth/internal/client"
)

// errUnverifiedArtifact refuses to save a download the server did not
// describe with a digest: such a server predates #1011 and may have truncated
// the body without saying so.
var errUnverifiedArtifact = errors.New("the server sent no " + client.ArtifactSHA256Header +
	" header, so the download cannot be verified; it predates #1011 and may truncate large artifacts. Upgrade the server")

// saveArtifactFile writes an artifact to destination through a temporary file
// in the same directory and renames it into place only after fill succeeds,
// so a failed or unverified download never leaves a plausible partial file.
func saveArtifactFile(destination string, fill func(io.Writer) error) error {
	directory, base := filepath.Split(destination)
	if base == "" {
		return fmt.Errorf("%w: --output must name a file, not a directory", errUsage)
	}
	if directory == "" {
		directory = "."
	}
	temporary, err := os.CreateTemp(directory, "."+base+".partial-*")
	if err != nil {
		return fmt.Errorf("create output: %w", err)
	}
	name := temporary.Name()
	committed := false
	defer func() {
		if !committed {
			_ = temporary.Close()
			_ = os.Remove(name)
		}
	}()
	if err := fill(temporary); err != nil {
		return err
	}
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("sync output: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close output: %w", err)
	}
	if err := os.Rename(name, destination); err != nil {
		_ = os.Remove(name)
		return fmt.Errorf("rename output: %w", err)
	}
	committed = true
	return nil
}

func reportSavedArtifact(destination string, receipt client.ArtifactReceipt) {
	fmt.Fprintf(os.Stderr, "saved %s (%d bytes, sha256 %s, verified)\n", destination, receipt.Bytes, receipt.SHA256)
}
