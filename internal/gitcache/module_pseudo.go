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
	ids := strings.Fields(matches)
	if len(ids) != 1 || ValidateSHA(ids[0]) != nil || !strings.HasPrefix(ids[0], revision) {
		return "", time.Time{}, ErrModulePseudoVersionUnavailable
	}
	source, err := c.modulePseudoCommit(ctx, path, ids[0])
	if err != nil {
		return "", time.Time{}, err
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
	args := []string{"for-each-ref", "--format=%(refname)", "--contains=" + source, "refs/heads/"}
	if allowNonDefault {
		args = append(args, "refs/tags/")
	}
	refs, err := c.capture(ctx, path, args...)
	if err != nil {
		return "", time.Time{}, err
	}
	for _, ref := range strings.Split(refs, "\n") {
		if ref == "refs/heads/"+branch {
			return source, timestamp, nil
		}
		if !allowNonDefault {
			continue
		}
		if strings.HasPrefix(ref, "refs/heads/") {
			return source, timestamp, nil
		}
		tag, tagRef := strings.CutPrefix(ref, "refs/tags/")
		if tagRef && module.CanonicalVersion(tag) == tag && !module.IsPseudoVersion(tag) && module.Check(modulePath, tag) == nil {
			return source, timestamp, nil
		}
	}
	return "", time.Time{}, ErrModulePseudoVersionUnavailable
}

func (c *Cache) modulePseudoCommit(ctx context.Context, path, query string) (string, error) {
	output := &boundedio.Buffer{Limit: 512, LimitError: errBlobTooLarge}
	if err := c.run(ctx, commandSpec{dir: path, args: []string{"cat-file", "--batch-check=%(objectname) %(objecttype)"}, stdin: strings.NewReader(query + "\n"), stdout: output}); err != nil {
		return "", err
	}
	fields := strings.Fields(string(output.Bytes()))
	if len(fields) == 2 && (fields[1] == "missing" || fields[1] == "ambiguous") {
		return "", ErrModulePseudoVersionUnavailable
	}
	if len(fields) != 2 || ValidateSHA(fields[0]) != nil {
		return "", errors.New("invalid module object response")
	}
	if fields[1] != "commit" {
		return "", ErrModulePseudoVersionUnavailable
	}
	return fields[0], nil
}
