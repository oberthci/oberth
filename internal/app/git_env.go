package app

import (
	"errors"
	"fmt"
	"net/url"
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

// SSHConfigEntry describes one per-host block in a generated SSH config file.
type SSHConfigEntry struct {
	Host         string
	Port         string
	IdentityFile string
}

// validateSSHConfigHost rejects any hostname that could not have come from a
// well-formed upstream URL. OpenSSH config directives are newline- and
// whitespace-delimited and Host arguments are glob patterns, so anything
// outside a conservative hostname alphabet is refused rather than escaped.
// net/url already rejects control characters and refuses percent-escapes that
// would decode to them inside the host component; this check keeps
// BuildSSHConfig safe even for callers that skip URL parsing.
func validateSSHConfigHost(host string) error {
	if host == "" {
		return errors.New("host must not be empty")
	}
	for _, r := range host {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.' || r == '-' || r == '_' || r == ':':
		default:
			return fmt.Errorf("host contains disallowed character %q", r)
		}
	}
	return nil
}

// BuildSSHConfig generates an OpenSSH config file body from per-host entries
// and a fallback identity. The result is safe for use with `ssh -F <path>`.
// Every host block enforces the same fail-closed transport as GitSSHCommand:
// batch mode, single identity, strict host-key checking, and no global known
// hosts. The fallback block matches every host WITHOUT a per-host entry and
// uses the default identity; per-host names are negated from the fallback
// pattern because OpenSSH accumulates IdentityFile directives across all
// matching blocks — without the negation, the shared fallback key would also
// be offered to per-key hosts on retry, silently masking a revoked per-host
// key and disclosing the shared public key to a forge that should only ever
// see its own. Two upstreams sharing one hostname cannot have distinct keys
// over plain SSH host matching; the first entry's block wins Port and its
// identity is offered first.
func BuildSSHConfig(entries []SSHConfigEntry, defaultKeyPath, knownHostsPath string) (string, error) {
	if err := validateOperatorPath(defaultKeyPath); err != nil {
		return "", fmt.Errorf("app: default SSH key: %w", err)
	}
	if err := validateOperatorPath(knownHostsPath); err != nil {
		return "", fmt.Errorf("app: known_hosts: %w", err)
	}
	var builder strings.Builder
	seenHosts := make(map[string]bool)
	negations := make([]string, 0, len(entries))
	for _, entry := range entries {
		if err := validateSSHConfigHost(entry.Host); err != nil {
			return "", fmt.Errorf("app: SSH config entry host %q: %w", entry.Host, err)
		}
		for _, r := range entry.Port {
			if r < '0' || r > '9' {
				return "", fmt.Errorf("app: SSH config entry %s port %q is not numeric", entry.Host, entry.Port)
			}
		}
		if err := validateOperatorPath(entry.IdentityFile); err != nil {
			return "", fmt.Errorf("app: SSH config entry %s identity: %w", entry.Host, err)
		}
		if !seenHosts[entry.Host] {
			seenHosts[entry.Host] = true
			negations = append(negations, "!"+entry.Host)
		}
		fmt.Fprintf(&builder, "Host %s\n", entry.Host)
		if entry.Port != "" && entry.Port != "22" {
			fmt.Fprintf(&builder, "  Port %s\n", entry.Port)
		}
		// OpenSSH double-quotes preserve whitespace and '#' in path values.
		// validateOperatorPath already rejects '"' and '\' so quoting is safe.
		fmt.Fprintf(&builder, "  IdentityFile \"%s\"\n", entry.IdentityFile)
		fmt.Fprintf(&builder, "  IdentitiesOnly yes\n")
		fmt.Fprintf(&builder, "  BatchMode yes\n")
		fmt.Fprintf(&builder, "  StrictHostKeyChecking yes\n")
		fmt.Fprintf(&builder, "  GlobalKnownHostsFile /dev/null\n")
		fmt.Fprintf(&builder, "  UserKnownHostsFile \"%s\"\n", knownHostsPath)
		fmt.Fprintf(&builder, "\n")
	}
	fmt.Fprintf(&builder, "Host %s\n", strings.Join(append([]string{"*"}, negations...), " "))
	// Same quoting for the fallback block.
	fmt.Fprintf(&builder, "  IdentityFile \"%s\"\n", defaultKeyPath)
	fmt.Fprintf(&builder, "  IdentitiesOnly yes\n")
	fmt.Fprintf(&builder, "  BatchMode yes\n")
	fmt.Fprintf(&builder, "  StrictHostKeyChecking yes\n")
	fmt.Fprintf(&builder, "  GlobalKnownHostsFile /dev/null\n")
	fmt.Fprintf(&builder, "  UserKnownHostsFile \"%s\"\n", knownHostsPath)
	return builder.String(), nil
}

// GitSSHCommandFromConfig returns the GIT_SSH_COMMAND value that uses a
// generated SSH config file instead of inline arguments.
func GitSSHCommandFromConfig(configPath string) (string, error) {
	if err := validateOperatorPath(configPath); err != nil {
		return "", fmt.Errorf("app: SSH config path: %w", err)
	}
	return shellQuote("ssh") + " " + shellQuote("-F") + " " + shellQuote(configPath), nil
}

// UpstreamSSHHost extracts the SSH hostname and port from an upstream base URL.
// Returns empty strings for non-SSH upstreams instead of an error so callers
// can skip local upstreams without branching.
func UpstreamSSHHost(baseURL string) (string, string) {
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Scheme != "ssh" {
		return "", ""
	}
	host := parsed.Hostname()
	port := parsed.Port()
	if port == "" {
		port = "22"
	}
	return host, port
}

// ValidateUpstreamName enforces the registration-time contract for upstream
// names. The name becomes a Secret data key (`<keyFile>-<name>`), a file name
// on the projected Secret volume, an IdentityFile path element inside the
// generated SSH config, and an SSA field-manager suffix — so it is restricted
// to a DNS-1123 label: lowercase alphanumerics and hyphens, starting and
// ending alphanumeric, at most 53 characters. Everything else is refused at
// the door instead of being escaped downstream.
func ValidateUpstreamName(name string) error {
	if name == "" {
		return errors.New("app: upstream name must not be empty")
	}
	if len(name) > 53 {
		return errors.New("app: upstream name exceeds 53 characters")
	}
	for index, r := range name {
		lower := r >= 'a' && r <= 'z'
		digit := r >= '0' && r <= '9'
		hyphen := r == '-'
		if !lower && !digit && !hyphen {
			return fmt.Errorf("app: upstream name contains disallowed character %q; use lowercase letters, digits, and hyphens", r)
		}
		if hyphen && (index == 0 || index == len(name)-1) {
			return errors.New("app: upstream name must start and end with a letter or digit")
		}
	}
	return nil
}

// UpstreamKeyDataKey derives the per-upstream private-key data key inside the
// configured upstream-key Secret. The same string is the file name projected
// by the Secret volume mount, so the server and admin CLI resolve the key at
// filepath.Join(filepath.Dir(--upstream-key), dataKey). The public half lives
// beside it under dataKey + ".pub".
func UpstreamKeyDataKey(keyFile, upstreamName string) string {
	return keyFile + "-" + upstreamName
}

// ValidUpstreamKeyName reports whether a stored per-upstream key name from
// the database is safe to use as a single path element. Rows are written
// through ValidateUpstreamName-guarded registration, but path construction
// treats the database as untrusted input and re-checks before joining.
func ValidUpstreamKeyName(keyName string) bool {
	if keyName == "" || keyName == "." || keyName == ".." {
		return false
	}
	if filepath.Base(keyName) != keyName {
		return false
	}
	for _, r := range keyName {
		lower := r >= 'a' && r <= 'z'
		digit := r >= '0' && r <= '9'
		if !lower && !digit && r != '-' && r != '_' && r != '.' {
			return false
		}
	}
	return true
}
