package gitcache

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/oberthci/oberth/internal/boundedio"
)

type RetirementSource struct {
	Ref, SHA, Path string
	Body           []byte
}

// RetirementSources requires a fresh upstream view and includes unpublished
// public branch heads. Historical tags are inspected only when a live pipeline
// pins them as fragments. Symlinks, submodules and incomplete inventories refuse.
func (c *Cache) RetirementSources(ctx context.Context, input string) ([]RetirementSource, error) {
	ready, err := c.Ensure(ctx, input)
	if err != nil {
		return nil, err
	}
	if ready.Stale {
		return nil, errors.New("retirement requires fresh repository history")
	}
	lock := c.repoLock(ready.Path)
	if err := lock.RLockContext(ctx); err != nil {
		return nil, err
	}
	defer lock.RUnlock()
	refs, err := c.listRefsAtMost(ctx, ready.Path, 1024, "refs/heads/", upstreamRefPrefix+"heads/")
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(refs))
	for name := range refs {
		names = append(names, name)
	}
	slices.Sort(names)
	var out []RetirementSource
	bySHA := map[string][]RetirementSource{}
	total := 0
	for _, ref := range names {
		sha := refs[ref]
		files, found := bySHA[sha]
		if !found {
			var err error
			files, err = c.retirementSourceTree(ctx, ready.Path, sha)
			if err != nil {
				return nil, fmt.Errorf("ref %s: %w", ref, err)
			}
			for _, source := range files {
				total += len(source.Body)
			}
			if total > 16<<20 {
				return nil, errors.New("retirement source inventory exceeds 16 MiB")
			}
			bySHA[sha] = files
		}
		for _, source := range files {
			source.Ref = ref
			out = append(out, source)
			if len(out) > 8192 {
				return nil, errors.New("retirement source inventory exceeds 8192 entries")
			}
		}
	}
	return out, nil
}

func (c *Cache) retirementSourceTree(ctx context.Context, path, sha string) ([]RetirementSource, error) {
	if err := ValidateSHA(sha); err != nil {
		return nil, err
	}
	output := &boundedio.Buffer{Limit: 512 << 10, LimitError: errBlobTooLarge}
	if err := c.run(ctx, commandSpec{dir: path, args: []string{"ls-tree", "-r", "-z", sha, "--", ".oberth"}, stdout: output}); err != nil {
		return nil, err
	}
	var out []RetirementSource
	for _, entry := range bytes.Split(output.Bytes(), []byte{0}) {
		if len(entry) == 0 {
			continue
		}
		metadata, name, ok := strings.Cut(string(entry), "\t")
		fields := strings.Fields(metadata)
		if !ok || len(fields) != 3 || fields[1] != "blob" || (fields[0] != "100644" && fields[0] != "100755") {
			return nil, fmt.Errorf("retirement source %s must be a regular Git blob", name)
		}
		if !strings.HasSuffix(name, ".yaml") && !strings.HasSuffix(name, ".yml") {
			continue
		}
		body, exists, err := c.readOptionalBlobLocked(ctx, path, sha, name, 1<<20)
		if err != nil {
			return nil, fmt.Errorf("source %s: %w", name, err)
		}
		if !exists {
			return nil, fmt.Errorf("source %s disappeared", name)
		}
		out = append(out, RetirementSource{SHA: sha, Path: name, Body: body})
		if len(out) > 256 {
			return nil, errors.New("retirement source tree exceeds 256 YAML files")
		}
	}
	return out, nil
}
