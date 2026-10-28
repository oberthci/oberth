package gitcache

import (
	"bytes"
	"context"
	"fmt"
	"strings"
)

// Preserve the promotion policy while using native Git: both sides may change
// a path only when the resulting entry is identical. Content merges and rename
// guesses must not admit a promotion that previously required a rebase.
func (c *Cache) mergePromotionTrees(ctx context.Context, workspace, sourceSHA, target string) error {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	refuse := func() error { return fmt.Errorf("promotion paths conflict; rebase onto %s", target) }
	bases, err := c.capture(ctx, workspace, "merge-base", "--all", "HEAD", sourceSHA)
	if err != nil || len(strings.Fields(bases)) != 1 {
		return refuse()
	}
	baseline, err := c.promotionTree(ctx, workspace, strings.TrimSpace(bases))
	if err != nil {
		return err
	}
	left, err := c.promotionTree(ctx, workspace, "HEAD")
	if err != nil {
		return err
	}
	right, err := c.promotionTree(ctx, workspace, sourceSHA)
	if err != nil {
		return err
	}
	names := make(map[string]bool)
	for name := range baseline {
		names[name] = true
	}
	for name := range left {
		names[name] = true
	}
	for name := range right {
		names[name] = true
	}
	for name := range names {
		if left[name] != right[name] && left[name] != baseline[name] && right[name] != baseline[name] {
			return refuse()
		}
	}
	if err := c.run(ctx, commandSpec{dir: workspace, args: []string{
		"-c", "user.name=Oberth", "-c", "user.email=oberth@localhost", "-c", "commit.gpgSign=false", "-c", "core.hooksPath=/dev/null",
		"merge", "--no-ff", "--no-edit", "--no-gpg-sign", "-Xno-renames", "-m", fmt.Sprintf("Merge %s into %s", sourceSHA, target), sourceSHA,
	}}); err != nil {
		return refuse()
	}
	return nil
}

// NUL framing preserves literal Git filenames. The inventory is bounded and
// refuses oversize repositories rather than comparing a truncated tree.
func (c *Cache) promotionTree(ctx context.Context, workspace, ref string) (map[string]string, error) {
	output := &boundedGitInventory{}
	if err := c.run(ctx, commandSpec{dir: workspace, args: []string{"ls-tree", "-rz", "--full-tree", ref}, stdout: output}); err != nil {
		return nil, err
	}
	entries := make(map[string]string)
	for _, record := range bytes.Split(output.Bytes(), []byte{0}) {
		if len(record) == 0 {
			continue
		}
		entry, name, ok := bytes.Cut(record, []byte{'\t'})
		if !ok || len(name) == 0 {
			return nil, fmt.Errorf("invalid promotion tree inventory")
		}
		entries[string(name)] = string(entry)
	}
	return entries, nil
}
