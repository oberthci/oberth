package gitcache

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// Both adapters use this promotion policy. It intentionally makes no content
// merge or rename guesses: changes to the same path must produce the same
// entry, or the author must rebase. Thus switching adapters cannot change
// which conflicting promotion is admitted.
func (c *Cache) mergePromotionTrees(ctx context.Context, workspace, sourceSHA, target string) error {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	r, closeStorage, err := openGoRepository(workspace)
	if err != nil {
		return err
	}
	defer closeStorage()
	head, err := r.Head()
	if err != nil {
		return err
	}
	source := plumbing.NewHash(sourceSHA)
	base, err := conservativeMergeBase(ctx, r, head.Hash(), source)
	if err != nil {
		return fmt.Errorf("%w; rebase onto %s", err, target)
	}
	baseline, err := flattenMergeTree(ctx, r, base)
	if err != nil {
		return err
	}
	left, err := flattenMergeTree(ctx, r, head.Hash())
	if err != nil {
		return err
	}
	right, err := flattenMergeTree(ctx, r, source)
	if err != nil {
		return err
	}
	names := map[string]bool{}
	for name := range baseline {
		names[name] = true
	}
	for name := range left {
		names[name] = true
	}
	for name := range right {
		names[name] = true
	}
	ordered := make([]string, 0, len(names))
	for name := range names {
		ordered = append(ordered, name)
	}
	sort.Strings(ordered)
	merged := map[string]object.TreeEntry{}
	for _, name := range ordered {
		baseEntry, baseExists := baseline[name]
		a, aExists := left[name]
		b, bExists := right[name]
		var chosen object.TreeEntry
		exists := false
		switch {
		case equalMergeEntry(a, aExists, b, bExists):
			chosen, exists = a, aExists
		case equalMergeEntry(a, aExists, baseEntry, baseExists):
			chosen, exists = b, bExists
		case equalMergeEntry(b, bExists, baseEntry, baseExists):
			chosen, exists = a, aExists
		default:
			return fmt.Errorf("promotion path %q changed on both sides; rebase onto %s", name, target)
		}
		if exists {
			merged[name] = chosen
		}
	}
	root := &mergeTreeNode{children: map[string]*mergeTreeNode{}}
	for _, name := range ordered {
		entry, ok := merged[name]
		if !ok {
			continue
		}
		node := root
		parts := strings.Split(name, "/")
		for i, part := range parts {
			if node.entry != nil {
				return fmt.Errorf("promotion file/directory conflict at %q; rebase onto %s", name, target)
			}
			child := node.children[part]
			if child == nil {
				child = &mergeTreeNode{children: map[string]*mergeTreeNode{}}
				node.children[part] = child
			}
			node = child
			if i == len(parts)-1 {
				if len(node.children) > 0 {
					return fmt.Errorf("promotion file/directory conflict at %q; rebase onto %s", name, target)
				}
				copy := entry
				copy.Name = part
				node.entry = &copy
			}
		}
	}
	tree, err := writeMergeTree(ctx, r, root)
	if err != nil {
		return err
	}
	signature := object.Signature{Name: "Oberth", Email: "oberth@localhost", When: time.Now().UTC().Truncate(time.Second)}
	commit := &object.Commit{Author: signature, Committer: signature, Message: fmt.Sprintf("Merge %s into %s\n", sourceSHA, target), TreeHash: tree, ParentHashes: []plumbing.Hash{head.Hash(), source}}
	encoded := r.Storer.NewEncodedObject()
	if err := commit.Encode(encoded); err != nil {
		return err
	}
	hash, err := r.Storer.SetEncodedObject(encoded)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	work, err := r.Worktree()
	if err != nil {
		return err
	}
	return work.Checkout(&git.CheckoutOptions{Hash: hash, Force: true})
}
func equalMergeEntry(a object.TreeEntry, aExists bool, b object.TreeEntry, bExists bool) bool {
	return aExists == bExists && (!aExists || (a.Hash == b.Hash && a.Mode == b.Mode))
}
func flattenMergeTree(ctx context.Context, r *git.Repository, hash plumbing.Hash) (map[string]object.TreeEntry, error) {
	commit, err := r.CommitObject(hash)
	if err != nil {
		return nil, err
	}
	tree, err := commit.Tree()
	if err != nil {
		return nil, err
	}
	walk := object.NewTreeWalker(tree, true, nil)
	defer walk.Close()
	entries := map[string]object.TreeEntry{}
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		name, entry, err := walk.Next()
		if errors.Is(err, io.EOF) {
			return entries, nil
		}
		if err != nil {
			return nil, err
		}
		if entry.Mode != filemode.Dir {
			entries[name] = entry
		}
	}
}

type mergeTreeNode struct {
	entry    *object.TreeEntry
	children map[string]*mergeTreeNode
}

func writeMergeTree(ctx context.Context, r *git.Repository, node *mergeTreeNode) (plumbing.Hash, error) {
	if err := ctx.Err(); err != nil {
		return plumbing.ZeroHash, err
	}
	tree := &object.Tree{}
	for name, child := range node.children {
		if child.entry != nil {
			tree.Entries = append(tree.Entries, *child.entry)
			continue
		}
		hash, err := writeMergeTree(ctx, r, child)
		if err != nil {
			return plumbing.ZeroHash, err
		}
		tree.Entries = append(tree.Entries, object.TreeEntry{Name: name, Mode: filemode.Dir, Hash: hash})
	}
	sort.Sort(object.TreeEntrySorter(tree.Entries))
	encoded := r.Storer.NewEncodedObject()
	if err := tree.Encode(encoded); err != nil {
		return plumbing.ZeroHash, err
	}
	return r.Storer.SetEncodedObject(encoded)
}
func mergeAncestors(ctx context.Context, r *git.Repository, head plumbing.Hash) (map[plumbing.Hash][]plumbing.Hash, error) {
	ancestors := map[plumbing.Hash][]plumbing.Hash{}
	queue := []plumbing.Hash{head}
	for len(queue) > 0 {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		hash := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		if _, ok := ancestors[hash]; ok {
			continue
		}
		commit, err := r.CommitObject(hash)
		if err != nil {
			return nil, err
		}
		ancestors[hash] = commit.ParentHashes
		queue = append(queue, commit.ParentHashes...)
	}
	return ancestors, nil
}
func conservativeMergeBase(ctx context.Context, r *git.Repository, left, right plumbing.Hash) (plumbing.Hash, error) {
	a, err := mergeAncestors(ctx, r, left)
	if err != nil {
		return plumbing.ZeroHash, err
	}
	b, err := mergeAncestors(ctx, r, right)
	if err != nil {
		return plumbing.ZeroHash, err
	}
	best := map[plumbing.Hash]bool{}
	for hash := range a {
		if _, common := b[hash]; common {
			best[hash] = true
		}
	}
	for hash, parents := range a {
		if _, common := b[hash]; common {
			for _, parent := range parents {
				delete(best, parent)
			}
		}
	}
	if len(best) != 1 {
		return plumbing.ZeroHash, errors.New("promotion requires a unique merge base")
	}
	for hash := range best {
		return hash, nil
	}
	return plumbing.ZeroHash, errors.New("promotion has no merge base")
}
