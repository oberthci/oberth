package gitcache

import (
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
)

var (
	repoPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	shaPattern  = regexp.MustCompile(`^[0-9a-f]{40}$`)
	userPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
)

const maximumRefPartBytes = 255

// NormalizeRepo accepts the canonical SSH spellings /repo and repo.git while
// refusing owner prefixes, traversal, and filesystem syntax. Repository-to-org
// mapping belongs in the configured upstream resolver, never in a client path.
func NormalizeRepo(value string) (string, error) {
	if strings.ContainsAny(value, "\x00\r\n\\") {
		return "", errors.New("repository contains forbidden characters")
	}
	value = strings.TrimPrefix(value, "/")
	value = strings.TrimSuffix(value, ".git")
	if !repoPattern.MatchString(value) || value == "." || value == ".." || strings.Contains(value, "..") {
		return "", fmt.Errorf("invalid repository %q", value)
	}
	if strings.Contains(value, "/") || filepath.Base(value) != value {
		return "", fmt.Errorf("repository %q must not contain a path", value)
	}
	return value, nil
}

// ValidateSHA requires the exact SHA-1 object IDs used by the configured
// Forge. Abbreviations are an API convenience, never a mutation primitive.
func ValidateSHA(value string) error {
	if !shaPattern.MatchString(value) {
		return fmt.Errorf("SHA %q must be 40 lowercase hexadecimal characters", value)
	}
	return nil
}

func ValidateBranch(value string) error { return validateRefPart("branch", value) }
func ValidateTag(value string) error    { return validateRefPart("tag", value) }

// ValidateUpstream permits only explicit SSH/HTTPS URLs and absolute local
// paths used by hermetic tests. Git helper protocols, option-like values,
// embedded URL credentials, and control characters fail closed.
func ValidateUpstream(value string) error {
	lower := strings.ToLower(value)
	if value == "" || value != strings.TrimSpace(value) || strings.HasPrefix(value, "-") || strings.ContainsAny(value, "\x00\r\n") ||
		strings.Contains(lower, "%00") || strings.Contains(lower, "%0a") || strings.Contains(lower, "%0d") {
		return errors.New("upstream remote is empty or contains unsafe characters")
	}
	if filepath.IsAbs(value) {
		if filepath.Clean(value) != value {
			return errors.New("local upstream path must be clean and absolute")
		}
		return nil
	}
	parsed, err := url.Parse(value)
	if err != nil {
		return fmt.Errorf("parse upstream remote: %w", err)
	}
	if parsed.Scheme != "ssh" && parsed.Scheme != "https" {
		return fmt.Errorf("upstream scheme %q is not allowed", parsed.Scheme)
	}
	if parsed.Hostname() == "" || strings.HasPrefix(parsed.Hostname(), "-") || parsed.Path == "" || parsed.Path == "/" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("upstream URL requires a host and path and must not contain query or fragment data")
	}
	for _, part := range strings.Split(parsed.Path, "/") {
		if part == "." || part == ".." || strings.ContainsAny(part, "\x00\r\n") {
			return errors.New("upstream URL path contains traversal or control characters")
		}
	}
	if parsed.Scheme == "https" && parsed.User != nil {
		return errors.New("HTTPS upstream credentials must not be embedded in the URL")
	}
	if parsed.User != nil {
		if _, hasPassword := parsed.User.Password(); hasPassword {
			return errors.New("SSH upstream passwords must not be embedded in the URL")
		}
		if !userPattern.MatchString(parsed.User.Username()) {
			return errors.New("SSH upstream username contains unsafe characters")
		}
	}
	return nil
}

func validateRefPart(kind, value string) error {
	if value == "" || len(value) > maximumRefPartBytes {
		return fmt.Errorf("%s name has invalid length", kind)
	}
	if strings.HasPrefix(value, "-") || strings.HasPrefix(value, "/") || strings.HasSuffix(value, "/") ||
		strings.HasPrefix(value, "refs/") ||
		strings.HasSuffix(value, ".") || strings.Contains(value, "//") || strings.Contains(value, "..") ||
		strings.Contains(value, "@{") || strings.ContainsAny(value, " ~^:?*[\\\x00\r\n") {
		return fmt.Errorf("invalid %s name %q", kind, value)
	}
	for _, part := range strings.Split(value, "/") {
		if part == "" || part == "." || part == ".." || strings.HasSuffix(part, ".lock") {
			return fmt.Errorf("invalid %s name %q", kind, value)
		}
	}
	return nil
}
