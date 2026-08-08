package app

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/ssh"
)

// GitSSHCommand returns the fixed, non-interactive SSH transport used for all
// upstream Git commands. Operator paths are validated and shell-quoted because
// Git evaluates GIT_SSH_COMMAND through a shell.
func GitSSHCommand(privateKeyPath, knownHostsPath string) (string, error) {
	if err := validateOperatorFile(privateKeyPath, true); err != nil {
		return "", fmt.Errorf("app: upstream private key: %w", err)
	}
	if err := validateOperatorFile(knownHostsPath, false); err != nil {
		return "", fmt.Errorf("app: upstream known_hosts: %w", err)
	}
	return GitSSHCommandPaths(privateKeyPath, knownHostsPath)
}

// GitSSHCommandPaths builds the same fail-closed transport before bootstrap
// Secrets exist. Git will fail until both projected files appear; readiness
// separately requires GitSSHCommand's full file validation.
func GitSSHCommandPaths(privateKeyPath, knownHostsPath string) (string, error) {
	if err := validateOperatorPath(privateKeyPath); err != nil {
		return "", fmt.Errorf("app: upstream private key: %w", err)
	}
	if err := validateOperatorPath(knownHostsPath); err != nil {
		return "", fmt.Errorf("app: upstream known_hosts: %w", err)
	}
	arguments := []string{
		"ssh",
		"-F", "/dev/null",
		"-i", privateKeyPath,
		"-o", "BatchMode=yes",
		"-o", "IdentitiesOnly=yes",
		"-o", "StrictHostKeyChecking=yes",
		"-o", "GlobalKnownHostsFile=/dev/null",
		// OpenSSH reparses -o values after argv decoding. Preserve whitespace in
		// an otherwise validated absolute path with config-level quoting too.
		"-o", `UserKnownHostsFile="` + knownHostsPath + `"`,
	}
	quoted := make([]string, len(arguments))
	for index, argument := range arguments {
		quoted[index] = shellQuote(argument)
	}
	return strings.Join(quoted, " "), nil
}

// SSHPublicKeyFingerprint derives the SHA-256 public-key fingerprint of the
// upstream Git identity for read-only display. Only the fingerprint of the
// public half ever leaves this function; the private key bytes stay local and
// an encrypted or missing key is simply an error.
func SSHPublicKeyFingerprint(privateKeyPath string) (string, error) {
	if err := validateOperatorFile(privateKeyPath, true); err != nil {
		return "", fmt.Errorf("app: upstream private key: %w", err)
	}
	info, err := os.Stat(privateKeyPath)
	if err != nil {
		return "", fmt.Errorf("app: upstream private key: %w", err)
	}
	if info.Size() > 1<<20 {
		return "", errors.New("app: upstream private key exceeds the 1 MiB bound")
	}
	body, err := os.ReadFile(privateKeyPath) // #nosec G304 -- validated operator-supplied identity path.
	if err != nil {
		return "", fmt.Errorf("app: read upstream private key: %w", err)
	}
	signer, err := ssh.ParsePrivateKey(body)
	if err != nil {
		return "", fmt.Errorf("app: parse upstream private key: %w", err)
	}
	return ssh.FingerprintSHA256(signer.PublicKey()), nil
}

func validateOperatorFile(path string, private bool) error {
	if err := validateOperatorPath(path); err != nil {
		return err
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 {
		return errors.New("file must be a non-empty regular file")
	}
	if private && (info.Mode().Perm()&0o007 != 0 || info.Mode().Perm()&0o030 != 0) {
		return errors.New("private key may be owner/group readable but not writable or accessible by other")
	}
	return nil
}

func validateOperatorPath(path string) error {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path || strings.ContainsAny(path, "\x00\r\n$%\"\\") {
		return errors.New("path must be absolute, clean, and free of control/config-expansion characters")
	}
	return nil
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}
