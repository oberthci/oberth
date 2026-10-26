package gitcache

import (
	"context"
	"fmt"
	"path"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// Unlike go-git's TreeWalker, failed subtree loads are errors, never EOF.
// Depth-first frames retain only one tree per depth and bound hostile nesting.
func walkGoTree(ctx context.Context, r *git.Repository, root *object.Tree, visit func(string, object.TreeEntry) error) error {
	type frame struct {
		prefix string
		tree   *object.Tree
		next   int
	}
	stack := []frame{{tree: root}}
	active := map[plumbing.Hash]bool{root.Hash: true}
	for len(stack) > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		top := &stack[len(stack)-1]
		if top.next == len(top.tree.Entries) {
			delete(active, top.tree.Hash)
			stack = stack[:len(stack)-1]
			continue
		}
		entry := top.tree.Entries[top.next]
		top.next++
		name := path.Join(top.prefix, entry.Name)
		if err := visit(name, entry); err != nil {
			return err
		}
		if entry.Mode != filemode.Dir {
			continue
		}
		if len(stack) >= 1024 || active[entry.Hash] {
			return fmt.Errorf("invalid Git tree depth/cycle at %s", name)
		}
		child, err := r.TreeObject(entry.Hash)
		if err != nil {
			return fmt.Errorf("read Git subtree %s (%s): %w", name, entry.Hash, err)
		}
		active[entry.Hash] = true
		stack = append(stack, frame{prefix: name, tree: child})
	}
	return nil
}
