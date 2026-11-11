package installer

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
)

func checkAgeRecipient(ctx context.Context, recipient string) error {
	command := exec.CommandContext(ctx, "age", "-r", recipient) //nolint:gosec // G204: public recipient, fixed executable
	command.Stdin = bytes.NewReader(nil)
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	if err := command.Run(); err != nil {
		return errors.New("cannot encrypt for age recipient: install age and provide a valid recipient; nothing was installed")
	}
	return nil
}

func (s *secretsFile) enableEncryption(ctx context.Context, recipient string) error {
	info, err := os.Stat(filepath.Dir(s.path))
	if err != nil || info.Mode().Perm()&0o022 != 0 {
		return errors.New("encrypted secrets file requires a directory writable only by its owner")
	}
	s.ctx, s.recipient = ctx, recipient
	return nil
}

// Each complete ciphertext snapshot is synced before replacing the owned file.
// No plaintext file exists and earlier credentials survive a failed encryption.
func (s *secretsFile) writeEncrypted() error {
	temp, err := os.CreateTemp(filepath.Dir(s.path), ".oberth-secrets-*")
	if err != nil {
		return errors.New("cannot reserve encrypted secrets snapshot")
	}
	defer func() { _ = os.Remove(temp.Name()) }()
	transferred := false
	defer func() {
		if !transferred {
			_ = temp.Close()
		}
	}()
	command := exec.CommandContext(s.ctx, "age", "-r", s.recipient) //nolint:gosec // G204: fixed executable and public recipient
	command.Stdin = bytes.NewReader(s.plaintext)
	command.Stdout = temp
	command.Stderr = io.Discard
	if err := command.Run(); err != nil {
		return errors.New("age encryption failed; the preceding encrypted snapshot is preserved")
	}
	if err := temp.Sync(); err != nil {
		return errors.New("cannot sync encrypted secrets snapshot")
	}
	previous, err := s.file.Stat()
	current, currentErr := os.Lstat(s.path)
	if err != nil || currentErr != nil || !os.SameFile(previous, current) {
		return errors.New("secrets file was replaced; refusing to overwrite it")
	}
	if err := os.Rename(temp.Name(), s.path); err != nil {
		return errors.New("cannot commit encrypted secrets snapshot")
	}
	_ = s.file.Close()
	s.file = temp
	transferred = true
	directory, err := os.Open(filepath.Dir(s.path))
	if err != nil {
		return errors.New("cannot open encrypted secrets directory")
	}
	defer func() { _ = directory.Close() }()
	if err := directory.Sync(); err != nil {
		return errors.New("cannot sync encrypted secrets directory")
	}
	return nil
}
