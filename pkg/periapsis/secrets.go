package periapsis

import (
	"fmt"
	"strings"
)

const maxSecretStorePathBytes = 512

// ValidateSecretStorePath enforces the syntax shared by repository declarations, the
// administrator allowlist, and the fetching client: a clean, relative,
// slash-separated KV API path with a conservative character set. Vault system
// backends are rejected outright; a release secret read never targets auth,
// sys, or identity endpoints.
func ValidateSecretStorePath(path string) error {
	if path == "" || len(path) > maxSecretStorePathBytes {
		return fmt.Errorf("secret store path must contain 1-%d bytes", maxSecretStorePathBytes)
	}
	for _, segment := range strings.Split(path, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return fmt.Errorf("secret store path %q must use clean non-empty segments", path)
		}
		if !validSecretStorePathSegment(segment) {
			return fmt.Errorf("secret store path %q may use only ASCII letters, digits, dots, dashes, underscores, and slashes", path)
		}
	}
	first := path
	if index := strings.IndexByte(path, '/'); index >= 0 {
		first = path[:index]
	}
	switch first {
	case "auth", "sys", "identity", "cubbyhole":
		return fmt.Errorf("secret store path %q targets the reserved %q backend; only KV secret paths are allowed", path, first)
	}
	return nil
}

// validSecretStorePathSegment reports whether one already-nonempty path
// segment stays within the conservative shared charset.
func validSecretStorePathSegment(segment string) bool {
	for _, character := range []byte(segment) {
		valid := character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' || character == '-' || character == '_' || character == '.'
		if !valid {
			return false
		}
	}
	return true
}

// Hierarchical upstream-scoped secret paths -----------------------------------

const (
	upstreamNamespaceRoot    = "oberth"
	upstreamNamespaceSegment = "upstream"
	kvDataSegment            = "data"

	// UpstreamSecretPathPrefix is the virtual declared prefix for release
	// store-sourced secrets:
	//
	//	oberth/upstream/<org>/<secret>              release org-scoped
	//	oberth/upstream/<org>/<repo>/<secret>       release repo-scoped
	UpstreamSecretPathPrefix = upstreamNamespaceRoot + "/" + upstreamNamespaceSegment + "/"
)

// UpstreamSecretPath is one classified oberth/upstream/ declaration. Repo is
// empty for an org-scoped path.
type UpstreamSecretPath struct {
	Org    string
	Repo   string
	Secret string
}

// Scope names the authorization scope for errors and audit records.
func (path UpstreamSecretPath) Scope() string {
	if path.Repo == "" {
		return "org"
	}
	return "repo"
}

// Declared reconstructs the canonical declared (virtual) spelling.
func (path UpstreamSecretPath) Declared() string {
	if path.Repo == "" {
		return UpstreamSecretPathPrefix + path.Org + "/" + path.Secret
	}
	return UpstreamSecretPathPrefix + path.Org + "/" + path.Repo + "/" + path.Secret
}

// FetchPath maps the virtual declared form onto the KV v2 API read path under
// the server's configured mount. The transform is a constant-prefix rewrite of
// already segment-validated input; declared text can never steer the fetch
// outside the mount's data/upstream subtree.
func (path UpstreamSecretPath) FetchPath(kvMount string) string {
	suffix := path.Org + "/" + path.Secret
	if path.Repo != "" {
		suffix = path.Org + "/" + path.Repo + "/" + path.Secret
	}
	return kvMount + "/" + kvDataSegment + "/" + upstreamNamespaceSegment + "/" + suffix
}

// Authorize enforces the hierarchical scoping rule against the server-derived
// repository identity: the org segment must equal the registered upstream base
// URL's trailing org segment, and a repo-scoped path additionally requires the
// exact catalog repository name.
func (path UpstreamSecretPath) Authorize(org, repo string) error {
	if org == "" || !validSecretStorePathSegment(org) || org == "." || org == ".." {
		return fmt.Errorf("secret store path %q is upstream-scoped, but this repository's upstream registration defines no usable org (%q); register the upstream with an org-scoped base URL such as ssh://git@forge.example/<org>", path.Declared(), org)
	}
	if path.Org != org {
		return fmt.Errorf("secret store path %q names org %q, but this repository belongs to org %q", path.Declared(), path.Org, org)
	}
	if path.Repo != "" && path.Repo != repo {
		return fmt.Errorf("secret store path %q is scoped to repository %s/%s, but this release runs for repository %s/%s", path.Declared(), path.Org, path.Repo, org, repo)
	}
	return nil
}

// ParseUpstreamSecretStorePath validates generic secret store path syntax and
// classifies the path. It returns (parsed, true, nil) for a well-formed
// oberth/upstream/ path, (zero, false, nil) for a path outside the virtual
// namespace, and an error for invalid syntax or a malformed upstream structure.
func ParseUpstreamSecretStorePath(path string) (UpstreamSecretPath, bool, error) {
	if err := ValidateSecretStorePath(path); err != nil {
		return UpstreamSecretPath{}, false, err
	}
	segments := strings.Split(path, "/")
	if len(segments) >= 3 && segments[0] == upstreamNamespaceRoot && segments[1] == kvDataSegment && segments[2] == upstreamNamespaceSegment {
		return UpstreamSecretPath{}, false, fmt.Errorf("secret store path %q spells the reserved upstream subtree directly; declare it as %s<org>/<secret> or %s<org>/<repo>/<secret>", path, UpstreamSecretPathPrefix, UpstreamSecretPathPrefix)
	}
	if len(segments) < 2 || segments[0] != upstreamNamespaceRoot || segments[1] != upstreamNamespaceSegment {
		return UpstreamSecretPath{}, false, nil
	}
	switch len(segments) {
	case 4:
		return UpstreamSecretPath{Org: segments[2], Secret: segments[3]}, true, nil
	case 5:
		return UpstreamSecretPath{Org: segments[2], Repo: segments[3], Secret: segments[4]}, true, nil
	default:
		return UpstreamSecretPath{}, false, fmt.Errorf("secret store path %q must be %s<org>/<secret> (org-scoped) or %s<org>/<repo>/<secret> (repo-scoped)", path, UpstreamSecretPathPrefix, UpstreamSecretPathPrefix)
	}
}
