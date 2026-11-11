package installer

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// A secrets file receives the credentials an install creates exactly once:
// the OpenBao root token and unseal key of a new production store, and the
// bearer token of the first uplink. It is the path for installs run by a
// script or an AI agent, whose output is a log or a transcript: the values go
// to an owner-only file and the output names only its path and SHA-256.
//
// The file is created before anything is installed, so a path that cannot be
// used stops the install while nothing exists that could be lost, and it is
// removed again when the run created no credential.

// secretsFileKeys names each credential in the file. The names are stable:
// the install output tells the operator which line to read.
var secretsFileKeys = map[string]string{ //nolint:gosec // G101: line names in the secrets file, not credentials
	"Root token":   "OPENBAO_ROOT_TOKEN",
	"Unseal key":   "OPENBAO_UNSEAL_KEY",
	"Bearer token": "OBERTH_UPLINK_TOKEN",
}

// SecretsFileTokenKey is the line of the secrets file that holds the first
// uplink's bearer token.
const SecretsFileTokenKey = "OBERTH_UPLINK_TOKEN" //nolint:gosec // G101: a line name in the secrets file, not a credential

func secretsFileKey(label string) string {
	if key, ok := secretsFileKeys[label]; ok {
		return key
	}
	if rest, ok := strings.CutPrefix(label, "Unseal key "); ok {
		return "OPENBAO_UNSEAL_KEY_" + rest
	}
	return strings.ToUpper(strings.NewReplacer(" ", "_", "-", "_").Replace(label))
}

type secretsFile struct {
	path    string
	file    *os.File
	written int
	err     error
}

// CheckSecretsFilePath refuses a secrets-file path that would put credentials
// where they do not belong: an existing file (never overwritten), a symbolic
// link, a missing directory, or any place inside a git worktree, where one
// `git add .` publishes them.
func CheckSecretsFilePath(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", errors.New("--secrets-file needs a path")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	if _, err := os.Lstat(abs); err == nil {
		return "", fmt.Errorf("--secrets-file %s already exists; it is never overwritten: move it away or name a new file", abs)
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("--secrets-file %s: %w", abs, err)
	}
	dir, err := filepath.EvalSymlinks(filepath.Dir(abs))
	if err != nil {
		return "", fmt.Errorf("--secrets-file directory %s: %w", filepath.Dir(abs), err)
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return "", fmt.Errorf("--secrets-file directory %s is not a directory", dir)
	}
	for parent := dir; ; parent = filepath.Dir(parent) {
		if _, err := os.Lstat(filepath.Join(parent, ".git")); err == nil {
			return "", fmt.Errorf("--secrets-file %s is inside the git worktree %s; credentials must never be one `git add` away from a commit: choose a path outside it", abs, parent)
		}
		if filepath.Dir(parent) == parent {
			break
		}
	}
	return filepath.Join(dir, filepath.Base(abs)), nil
}

// createSecretsFile reserves path as a new owner-only file. O_EXCL refuses a
// file created since the check, O_NOFOLLOW a symbolic link planted there, and
// the explicit chmod makes the mode 0600 whatever the umask.
func createSecretsFile(path string) (*secretsFile, error) {
	checked, err := CheckSecretsFilePath(path)
	if err != nil {
		return nil, err
	}
	// The operator names the file; CheckSecretsFilePath refused existing
	// files, links and git worktrees.
	file, err := os.OpenFile(checked, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600) //nolint:gosec // G304: operator-named path, checked above
	if err != nil {
		return nil, fmt.Errorf("create --secrets-file %s: %w", checked, err)
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		_ = os.Remove(checked)
		return nil, fmt.Errorf("restrict --secrets-file %s: %w", checked, err)
	}
	return &secretsFile{path: checked, file: file}, nil
}

// add writes one credential and syncs it to disk at once: a later failure of
// the install must not lose a credential that exists nowhere else.
func (s *secretsFile) add(label, value string) {
	if s.err != nil {
		return
	}
	var line strings.Builder
	if s.written == 0 {
		fmt.Fprintf(&line, "# Oberth install credentials, written %s. Shown nowhere else.\n"+
			"# Move this file off this machine (a password manager, an offline copy) and delete it here.\n",
			time.Now().UTC().Format(time.RFC3339))
	}
	fmt.Fprintf(&line, "%s=%s\n", secretsFileKey(label), strings.TrimSpace(value))
	if _, err := io.WriteString(s.file, line.String()); err != nil {
		s.err = err
		return
	}
	if err := s.file.Sync(); err != nil {
		s.err = err
		return
	}
	s.written++
}

// finish closes the file and reports it by path and SHA-256 only. A run that
// created no credential leaves no file behind.
func (s *secretsFile) finish(w io.Writer) {
	// The empty file is removed by name, so make sure the name still is the
	// file this run created: one that was put there since belongs to the
	// operator and stays.
	created, createdErr := s.file.Stat()
	closeErr := s.file.Close()
	if s.written == 0 && s.err == nil {
		if current, err := os.Lstat(s.path); createdErr == nil && err == nil && os.SameFile(created, current) {
			_ = os.Remove(s.path)
		}
		_, _ = fmt.Fprintf(w, "\nNo new credentials were created; %s was not kept.\n", s.path)
		return
	}
	if s.err == nil {
		s.err = closeErr
	}
	if s.err != nil {
		_, _ = fmt.Fprintf(w, "\nERROR: writing the credentials to %s failed after %d of them: %v. The ones not written are lost; a new production store must be installed again.\n", s.path, s.written, s.err)
		return
	}
	raw, err := os.ReadFile(s.path)
	if err != nil {
		_, _ = fmt.Fprintf(w, "\nCredentials written to %s (mode 0600); reading it back failed: %v\n", s.path, err)
		return
	}
	sum := sha256.Sum256(raw)
	clear(raw)
	_, _ = fmt.Fprintf(w, "\nCredentials written to %s (mode 0600, %d values, sha256 %s).\n"+
		"Nothing secret was printed. Move the file off this machine and delete it here; do not read it into a log or an AI session.\n",
		s.path, s.written, hex.EncodeToString(sum[:]))
}

// mayCreateCredentials reports whether an install run can create a credential
// that is shown once: a production secret store (its init), or onboarding,
// which registers the first uplink and receives its bearer token.
func (cfg Config) mayCreateCredentials(interactive bool) bool {
	return cfg.InstallSecretStore || cfg.InstallRekor || cfg.ForgeURL != "" || cfg.ForgeOrg != "" || interactive
}
