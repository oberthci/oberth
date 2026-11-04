package gitcache

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"golang.org/x/mod/module"

	"github.com/oberthci/oberth/internal/boundedio"
)

// ErrModulePseudoVersionUnavailable denotes a positively invalid revision or
// a commit outside the administrator's current reachability policy. IO and
// cancellation errors remain distinct and must not become negative cache hits.
var ErrModulePseudoVersionUnavailable = errors.New("pseudo-version commit or policy unavailable")

// ModulePseudoVersionSnapshot reads only existing mirrored objects. It never
// fetches a requested revision. Public branch/tag reachability, commit time and
// any base tag are checked under one context-aware repository read lock.
func (c *Cache) ModulePseudoVersionSnapshot(ctx context.Context, input, modulePath, version string, allowNonDefault bool) (string, time.Time, error) {
	revision, err := module.PseudoVersionRev(version)
	if err != nil || len(revision) != 12 || strings.ToLower(revision) != revision || module.CanonicalVersion(version) != version || module.Check(modulePath, version) != nil {
		return "", time.Time{}, ErrModulePseudoVersionUnavailable
	}
	if _, err = hex.DecodeString(revision); err != nil {
		return "", time.Time{}, ErrModulePseudoVersionUnavailable
	}
	want, err := module.PseudoVersionTime(version)
	if err != nil {
		return "", time.Time{}, ErrModulePseudoVersionUnavailable
	}
	base, err := module.PseudoVersionBase(version)
	if err != nil {
		return "", time.Time{}, ErrModulePseudoVersionUnavailable
	}
	_, path, err := c.modulePath(ctx, input)
	if err != nil {
		return "", time.Time{}, err
	}
	unlock, err := c.lockModuleRepository(ctx, path)
	if err != nil {
		return "", time.Time{}, err
	}
	defer unlock()
	if !c.isBare(ctx, path) {
		return "", time.Time{}, errors.New("module repository is not cached")
	}
	// Resolve object IDs rather than Git's ref DWIM: a branch named like a short
	// hash must never redirect the requested revision to different content.
	matches, err := c.capture(ctx, path, "rev-parse", "--disambiguate="+revision)
	if err != nil {
		return "", time.Time{}, err
	}
	var source string
	for _, id := range strings.Fields(matches) {
		if ValidateSHA(id) != nil || !strings.HasPrefix(id, revision) {
			return "", time.Time{}, ErrModulePseudoVersionUnavailable
		}
		object, kind, err := c.modulePseudoObject(ctx, path, id)
		if err != nil {
			return "", time.Time{}, err
		}
		if kind != "commit" {
			continue
		}
		if source != "" {
			return "", time.Time{}, ErrModulePseudoVersionUnavailable
		}
		source = object
	}
	if source == "" {
		return "", time.Time{}, ErrModulePseudoVersionUnavailable
	}
	rawTime, err := c.capture(ctx, path, "show", "-s", "--format=%ct", source, "--")
	if err != nil {
		return "", time.Time{}, err
	}
	seconds, err := strconv.ParseInt(rawTime, 10, 64)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("invalid module commit time: %w", err)
	}
	timestamp := time.Unix(seconds, 0).UTC()
	if !timestamp.Equal(want) {
		return "", time.Time{}, ErrModulePseudoVersionUnavailable
	}
	if base != "" {
		baseSource, err := c.modulePseudoCommit(ctx, path, "refs/tags/"+strings.TrimSuffix(base, "+incompatible")+"^{commit}")
		if err != nil {
			return "", time.Time{}, err
		}
		if baseSource == source {
			return "", time.Time{}, ErrModulePseudoVersionUnavailable
		}
		if err = c.run(ctx, commandSpec{dir: path, args: []string{"merge-base", "--is-ancestor", baseSource, source}}); err != nil {
			if ctx.Err() == nil && isExitCode(err, 1) {
				return "", time.Time{}, ErrModulePseudoVersionUnavailable
			}
			return "", time.Time{}, err
		}
	}
	branch, err := c.currentDefaultBranch(ctx, path)
	if err != nil {
		return "", time.Time{}, err
	}
	// Ancestry of the default branch needs no ref enumeration, so an old
	// commit reachable from thousands of refs cannot overflow capture output.
	defaultSource, err := c.modulePseudoCommit(ctx, path, "refs/heads/"+branch)
	if err != nil && !errors.Is(err, ErrModulePseudoVersionUnavailable) {
		return "", time.Time{}, err
	}
	if err == nil {
		err = c.run(ctx, commandSpec{dir: path, args: []string{"merge-base", "--is-ancestor", source, defaultSource}})
		if err == nil {
			return source, timestamp, nil
		}
		if ctx.Err() != nil || !isExitCode(err, 1) {
			return "", time.Time{}, err
		}
	}
	if !allowNonDefault {
		return "", time.Time{}, ErrModulePseudoVersionUnavailable
	}
	// Every public branch is eligible in opt-in mode; one matching object ID
	// suffices regardless of how many branches contain the commit.
	branchSource, err := c.capture(ctx, path, "for-each-ref", "--count=1", "--format=%(objectname)", "--contains="+source, "refs/heads/")
	if err != nil {
		return "", time.Time{}, err
	}
	if branchSource != "" {
		if err := ValidateSHA(branchSource); err != nil {
			return "", time.Time{}, err
		}
		return source, timestamp, nil
	}
	// Tags need module-version filtering. Use the existing 16 MiB bounded ref
	// reader, with cancellation on overflow, rather than the 64 KiB capture.
	refs, err := c.listRefs(ctx, path, "--contains="+source, "refs/tags/")
	if err != nil {
		return "", time.Time{}, err
	}
	for ref := range refs {
		tag, tagRef := strings.CutPrefix(ref, "refs/tags/")
		if tagRef && module.CanonicalVersion(tag) == tag && !module.IsPseudoVersion(tag) && module.Check(modulePath, tag) == nil {
			return source, timestamp, nil
		}
	}
	return "", time.Time{}, ErrModulePseudoVersionUnavailable
}

func (c *Cache) modulePseudoCommit(ctx context.Context, path, query string) (string, error) {
	object, kind, err := c.modulePseudoObject(ctx, path, query)
	if err != nil {
		return "", err
	}
	if kind != "commit" {
		return "", ErrModulePseudoVersionUnavailable
	}
	return object, nil
}

func (c *Cache) modulePseudoObject(ctx context.Context, path, query string) (string, string, error) {
	output := &boundedio.Buffer{Limit: 512, LimitError: errBlobTooLarge}
	if err := c.run(ctx, commandSpec{dir: path, args: []string{"cat-file", "--batch-check=%(objectname) %(objecttype)"}, stdin: strings.NewReader(query + "\n"), stdout: output}); err != nil {
		return "", "", err
	}
	fields := strings.Fields(string(output.Bytes()))
	if len(fields) == 2 && (fields[1] == "missing" || fields[1] == "ambiguous") {
		return "", "", ErrModulePseudoVersionUnavailable
	}
	if len(fields) != 2 || ValidateSHA(fields[0]) != nil {
		return "", "", errors.New("invalid module object response")
	}
	return fields[0], fields[1], nil
}
