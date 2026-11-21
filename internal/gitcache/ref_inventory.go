package gitcache

import (
	"bytes"
	"context"
	"fmt"
	"strings"

	"github.com/oberthci/oberth/internal/gitoid"
)

// walkRefs streams native names and object IDs without applying the author's
// public-name grammar. Recovery must also be able to undo an unchecked name.
func (c *Cache) walkRefs(ctx context.Context, store string, visit func(string, string) error) error {
	output := &refLines{visit: visit}
	err := c.run(ctx, command{dir: store,
		args: []string{"for-each-ref", "--exclude=refs/oberth/**", "--format=%(refname) %(objectname)"}, output: output})
	if err != nil {
		return err
	}
	if len(output.pending) != 0 {
		return refuse("ref inventory")
	}
	return nil
}

type refLines struct {
	pending []byte
	visit   func(string, string) error
}

func (r *refLines) Write(data []byte) (int, error) {
	r.pending = append(r.pending, data...)
	for {
		line, rest, ok := bytes.Cut(r.pending, []byte{'\n'})
		if !ok {
			break
		}
		name, oid, valid := strings.Cut(string(line), " ")
		if !valid || !gitoid.Valid(oid) {
			return 0, refuse("ref inventory")
		}
		if err := r.visit(name, oid); err != nil {
			return 0, err
		}
		r.pending = rest
	}
	if len(r.pending) > 64<<10 {
		return 0, refuse("ref inventory")
	}
	r.pending = bytes.Clone(r.pending)
	return len(data), nil
}
func (c *Cache) refInventory(ctx context.Context, store string) (map[string]string, error) {
	refs := make(map[string]string)
	size := 0
	err := c.walkRefs(ctx, store, func(name, oid string) error {
		size += len(name) + len(oid) + 8
		if size > reconciliationBytes {
			return &LimitError{Limit: reconciliationBytes}
		}
		refs[name] = oid
		return nil
	})
	return refs, err
}
func refConflict(refs map[string]string, name string) string {
	for existing := range refs {
		if strings.HasPrefix(existing, name+"/") || strings.HasPrefix(name, existing+"/") {
			return existing
		}
	}
	return ""
}
func (c *Cache) mirrorConflict(repo, name, existing string) {
	if c.config.Log != nil {
		c.config.Log(reasonMirrorConflict, fmt.Sprintf("%s: %s conflicts with %s", repo, name, existing))
	}
}
