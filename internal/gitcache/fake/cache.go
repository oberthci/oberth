// Package fake supplies in-memory object retention for the Keep contract.
//
// Spec: docs/plans/c6-source-delivery.md#inspections
package fake

import (
	"context"
	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/gitport"
	"github.com/oberthci/oberth/internal/sourcecontract"
	"sync"
)

type objectSet map[string]map[string]bool

func (s objectSet) add(repo, oid string) {
	if s[repo] == nil {
		s[repo] = make(map[string]bool)
	}
	s[repo][oid] = true
}

type Cache struct {
	mu               sync.Mutex
	commits, objects objectSet
	pins             map[string]objectSet
}

func New(commits []sourcecontract.Revision, objects map[string][]string) *Cache {
	c := &Cache{commits: make(objectSet), objects: make(objectSet), pins: make(map[string]objectSet)}
	for _, r := range commits {
		c.commits.add(r.Repository, r.Commit)
		c.objects.add(r.Repository, r.Commit)
	}
	for repo, ids := range objects {
		for _, oid := range ids {
			c.objects.add(repo, oid)
		}
	}
	return c
}
func (c *Cache) Keep(ctx context.Context, owner string, bindings []sourcecontract.Binding) error {
	if !gitcache.ValidOwner(owner) {
		return &gitport.AuthorRefusalError{Path: "source owner"}
	}
	if ctx.Err() != nil {
		return context.Cause(ctx)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	groups := make(map[string][]sourcecontract.Binding)
	for _, b := range bindings {
		if err := b.Validate(); err != nil {
			return err
		}
		groups[b.Revision.Repository] = append(groups[b.Revision.Repository], b)
	}
	for repo, group := range groups {
		var objects []string
		for _, b := range group {
			roots, err := c.bindingObjects(b)
			if err != nil {
				return err
			}
			objects = append(objects, roots...)
		}
		if c.pins[owner] == nil {
			c.pins[owner] = make(objectSet)
		}
		for _, oid := range objects {
			c.pins[owner].add(repo, oid)
		}
	}
	return nil
}
func (c *Cache) bindingObjects(b sourcecontract.Binding) ([]string, error) {
	commits := []string{b.Revision.Commit}
	for _, r := range []sourcecontract.Ref{b.Export.Ref, b.Export.DefaultBranch} {
		if r.CommitSHA != "" {
			commits = append(commits, r.CommitSHA)
		}
	}
	for _, oid := range commits {
		if !c.commits[b.Revision.Repository][oid] {
			return nil, &gitport.AuthorRefusalError{Path: "recorded source", Code: "source_missing"}
		}
	}
	if raw := b.Export.Ref.ObjectSHA; raw != "" {
		if !c.objects[b.Revision.Repository][raw] {
			return nil, &gitport.AuthorRefusalError{Path: "recorded source", Code: "source_missing"}
		}
		commits = append(commits, raw)
	}
	return commits, nil
}
func (c *Cache) Has(owner, repo, oid string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.pins[owner][repo][oid]
}
