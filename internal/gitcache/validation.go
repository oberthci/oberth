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

const (
	maximumRefPartBytes = 255

	// One accepted ref update becomes one durable receive event and can enqueue
	// one CI or release run. 256 leaves ample room for deliberate branch/tag
	// batches while bounding one receive, its outbox record, and restart replay.
	maximumReceiveRefUpdates = 256
	// Repeated small pushes are bounded separately from one receive. 4096 public
	// heads plus tags accommodates long-lived repositories without allowing an
	// authenticated actor to grow control-plane work indefinitely.
	maximumPublicRefs = 4096
	// One deletion-only recovery receive can inspect at most the public cap plus
	// its bounded command count. Larger legacy namespaces require operator-side
	// migration because one admitted receive cannot bring them under the cap.
	maximumReceiveSnapshotRefs = maximumPublicRefs + maximumReceiveRefUpdates
)

// ReservedUpstreamNames may not be used as upstream names because they would
// alias an existing security or structural namespace. For example, an upstream
// named "release" would overlap the release secret boundary
// (oberth/data/release/*); "data" would overlap the KV mount; "upstream" would
// overlap the hierarchical path prefix (oberth/upstream/*); "sys" would overlap
// potential system paths.
var ReservedUpstreamNames = map[string]bool{
	"release":        true,
	"data":           true,
	"upstream":       true,
	"sys":            true,
	"metadata":       true,
	"receive-outbox": true,
}

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

// ValidateSegment validates a single path segment (upstream name, org, or repo)
// against repoPattern and rejects traversal values. It is the common validator
// for all segments of a repository path.
func ValidateSegment(kind, value string) error {
	if !repoPattern.MatchString(value) || value == "." || value == ".." || strings.Contains(value, "..") {
		return fmt.Errorf("invalid %s %q in repository path", kind, value)
	}
	return nil
}

// ParseRepoPath accepts bare repository names ("repo"), org-qualified paths
// ("org/repo"), and upstream-qualified paths ("upstream/org/repo"). It returns
// the optional upstream name, optional org prefix, and the validated bare
// repository name. Resolution of the returned upstream/org against registered
// upstreams happens at a higher layer; this function performs only syntactic
// validation.
//
// Forms:
//   - 1 segment ("repo")              → upstream="", org="", repo
//   - 2 segments ("org/repo")         → upstream="", org, repo
//   - 3 segments ("upstream/org/repo") → upstream, org, repo
func ParseRepoPath(value string) (upstream, org, repo string, err error) {
	if strings.ContainsAny(value, "\x00\r\n\\") {
		return "", "", "", errors.New("repository path contains forbidden characters")
	}
	value = strings.TrimPrefix(value, "/")
	value = strings.TrimSuffix(value, ".git")

	parts := strings.SplitN(value, "/", 4)

	switch len(parts) {
	case 3:
		// upstream/org/repo — canonical 3-segment form.
		if parts[0] == "" || parts[1] == "" || parts[2] == "" {
			return "", "", "", fmt.Errorf("repository path %q has empty segments", value)
		}
		if err := ValidateSegment("upstream", parts[0]); err != nil {
			return "", "", "", err
		}
		if err := ValidateSegment("organization", parts[1]); err != nil {
			return "", "", "", err
		}
		if err := ValidateSegment("repository", parts[2]); err != nil {
			return "", "", "", err
		}
		return parts[0], parts[1], parts[2], nil

	case 2:
		// org/repo — shorthand.
		if parts[0] == "" || parts[1] == "" {
			return "", "", "", fmt.Errorf("repository path %q has empty segments", value)
		}
		if err := ValidateSegment("organization", parts[0]); err != nil {
			return "", "", "", err
		}
		if err := ValidateSegment("repository", parts[1]); err != nil {
			return "", "", "", err
		}
		return "", parts[0], parts[1], nil

	case 1:
		// Bare repo name.
		repo, err = NormalizeRepo(value)
		if err != nil {
			return "", "", "", err
		}
		return "", "", repo, nil

	default:
		// 4+ segments — reject.
		return "", "", "", fmt.Errorf("repository path %q has too many segments (maximum 3: upstream/org/repo)", value)
	}
}

// ValidateUpstreamName checks that a name is valid for use as a registered
// upstream name: it must match repoPattern, and must not be a reserved name.
func ValidateUpstreamName(name string) error {
	if err := ValidateSegment("upstream name", name); err != nil {
		return err
	}
	if ReservedUpstreamNames[strings.ToLower(name)] {
		return fmt.Errorf("upstream name %q is reserved", name)
	}
	if strings.HasSuffix(strings.ToLower(name), ".git") {
		return fmt.Errorf("upstream name %q must not end with .git", name)
	}
	return nil
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
		return fmt.Errorf("upstream scheme %q is not allowed (use ssh:// or https://)", parsed.Scheme)
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
