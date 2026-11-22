package gitcache

import (
	"context"
	"fmt"
	"strings"

	"github.com/oberthci/oberth/internal/sourcecontract"
)

func refCountError() error {
	return &RefusalError{reason: reasonRefCount, remedy: remedyRefCount, Field: "public refs"}
}

func (c *Cache) tracking(ctx context.Context, p string) (map[string]string, error) {
	return c.trackingAt(ctx, p, trackingPrefix, reconciliationRefs)
}
func (c *Cache) trackingAt(ctx context.Context, p, prefix string, limit int) (map[string]string, error) {
	data, err := c.capture(
		ctx,
		command{
			dir: p,
			args: []string{
				"for-each-ref",
				fmt.Sprintf("--count=%d", limit+1),
				"--format=%(refname) %(objectname)",
				prefix,
			},
		},
		int64(2<<20)*int64(limit)/sourcecontract.MaxRefs,
	)
	if err != nil {
		return nil, err
	}
	refs := make(map[string]string)
	for _, line := range strings.Split(string(data), "\n") {
		if line == "" {
			continue
		}
		f := strings.Fields(line)
		if len(f) != 2 {
			return nil, refuse("tracking ref")
		}
		name := "refs/" + strings.TrimPrefix(f[0], prefix)
		if err = sourcecontract.ValidatePublicRef(name); err != nil {
			return nil, refuse("tracking ref")
		}
		refs[name] = f[1]
	}
	if len(refs) > limit {
		return nil, refCountError()
	}
	return refs, nil
}

// A tracking ref is the last upstream-owned value. Public changes that diverge
// from it belong to accepted local pushes and are never overwritten by refresh.
type mirrorChange struct {
	before, after map[string]string
	repository    string
}

func (c *Cache) mirror(ctx context.Context, p string, change mirrorChange) ([]RefUpdate, error) {
	retained := make(map[string]string)
	for name, oid := range change.before {
		if change.after[name] != "" {
			retained[name] = oid
		}
	}
	var removed []RefUpdate
	if len(retained) != len(change.before) {
		var err error
		removed, err = c.mirrorGeneration(ctx, p, mirrorChange{before: change.before, after: retained,
			repository: change.repository})
		if err != nil {
			return nil, err
		}
		change.before = retained
	}
	updates, err := c.mirrorGeneration(ctx, p, change)
	return append(removed, updates...), err
}
func (c *Cache) mirrorGeneration(ctx context.Context, p string, change mirrorChange) ([]RefUpdate, error) {
	public, err := c.refInventory(ctx, p)
	if err != nil {
		return nil, err
	}
	names := make(map[string]bool)
	for name := range change.before {
		names[name] = true
	}
	for name := range change.after {
		names[name] = true
	}
	var native, updates []RefUpdate
	for name := range names {
		old, next := change.before[name], change.after[name]
		if old == next {
			continue
		}
		tracking := trackingName(name)
		native = append(native, RefUpdate{Ref: tracking, OldSHA: old, NewSHA: next})
		if public[name] != old {
			continue
		}
		if next != "" {
			if conflict := refConflict(public, name); conflict != "" {
				c.mirrorConflict(change.repository, name, conflict)
				continue
			}
		}
		update := RefUpdate{Ref: name, OldSHA: old, NewSHA: next}
		updates = append(updates, update)
		if old == "" {
			update.OldSHA = strings.Repeat("0", len(next))
		}
		native = append(native, update)
	}
	if err = markReconciliation(p, updates); err != nil {
		return nil, err
	}
	if err = c.refTransaction(ctx, p, native); err != nil {
		return nil, err
	}
	return updates, clearReconciliation(p)
}

func (c *Cache) seedTracking(ctx context.Context, p, repo string) error {
	refs, err := c.list(ctx, p, repo)
	if err != nil {
		return err
	}
	for len(refs) > 0 {
		batch := refs[:min(len(refs), MaxUpdates)]
		var names []string
		var tx strings.Builder
		for _, ref := range batch {
			names = append(names, trackingName(ref.Name))
			fmt.Fprintf(&tx, "update %s %s\n", trackingName(ref.Name), ref.ObjectSHA)
		}
		if err = c.run(
			ctx,
			command{
				dir:       p,
				args:      []string{"update-ref", "--stdin"},
				input:     strings.NewReader(tx.String()),
				refWrites: names,
			},
		); err != nil {
			return err
		}
		refs = refs[len(batch):]
	}
	return nil
}
