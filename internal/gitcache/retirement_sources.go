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

// RetirementSource bodies may share immutable Git blob storage within one
// inventory. Callers must not mutate Body. Ref/SHA/Path remain per declaration.
type RetirementSource struct {
	Ref, SHA, Path string
	Body           []byte
}

// RetirementSources requires a fresh upstream view and includes unpublished
// public branch heads. Symlinks, submodules and incomplete inventories refuse.
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
	inventory := retirementBlobInventory{bodies: map[string][]byte{}}
	for _, ref := range names {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		sha := refs[ref]
		files, found := bySHA[sha]
		if !found {
			var err error
			files, err = c.retirementSourceTree(ctx, ready.Path, sha, &inventory)
			if err != nil {
				return nil, fmt.Errorf("ref %s: %w", ref, err)
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

type retirementBlobInventory struct {
	bodies map[string][]byte
	bytes  int
}

func (c *Cache) retirementSourceTree(ctx context.Context, path, sha string, inventory *retirementBlobInventory) ([]RetirementSource, error) {
	if err := ValidateSHA(sha); err != nil {
		return nil, err
	}
	output := &boundedio.Buffer{Limit: 512 << 10, LimitError: errBlobTooLarge}
	if err := c.run(ctx, commandSpec{dir: path, args: []string{"ls-tree", "-r", "-z", sha, "--", ".oberth"}, stdout: output}); err != nil {
		return nil, err
	}
	var out []RetirementSource
	for _, entry := range bytes.Split(output.Bytes(), []byte{0}) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if len(entry) == 0 {
			continue
		}
		metadata, name, ok := strings.Cut(string(entry), "\t")
		fields := strings.Fields(metadata)
		if !ok || len(fields) != 3 || fields[1] != "blob" || ValidateSHA(fields[2]) != nil || (fields[0] != "100644" && fields[0] != "100755") {
			return nil, fmt.Errorf("retirement source %s must be a regular Git blob", name)
		}
		if !strings.HasSuffix(name, ".yaml") && !strings.HasSuffix(name, ".yml") {
			continue
		}
		if !strings.HasPrefix(name, ".oberth/") || validateBlobPath(name) != nil {
			return nil, fmt.Errorf("retirement source %q has an invalid path", name)
		}
		// Check every tree entry before reusing bytes: a cached blob must not
		// make a symlink, submodule or unsafe path admissible on another head.
		body, found := inventory.bodies[fields[2]]
		if !found {
			sink := &boundedio.Buffer{Limit: 1 << 20, LimitError: errBlobTooLarge}
			if err := c.run(ctx, commandSpec{dir: path, args: []string{"cat-file", "blob", fields[2]}, stdout: sink}); err != nil {
				return nil, fmt.Errorf("source %s: %w", name, errors.Join(err, sink.Err()))
			}
			if err := sink.Err(); err != nil {
				return nil, fmt.Errorf("source %s: %w", name, err)
			}
			if len(sink.Bytes()) > (16<<20)-inventory.bytes {
				return nil, errors.New("retirement source inventory exceeds 16 MiB")
			}
			// Compact the bounded read buffer before retaining it. All aliases
			// share these bytes; the budget counts each immutable blob once.
			body = bytes.Clone(sink.Bytes())
			inventory.bytes += len(body)
			inventory.bodies[fields[2]] = body
		}
		out = append(out, RetirementSource{SHA: sha, Path: name, Body: body})
		if len(out) > 256 {
			return nil, errors.New("retirement source tree exceeds 256 YAML files")
		}
	}
	return out, nil
}
