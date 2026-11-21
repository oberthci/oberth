package gitcache

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/oberthci/oberth/internal/sourcecontract"
	"github.com/oberthci/oberth/internal/sourcedelivery"
)

// OwnerPage is read from durable run state. Only owners that can still load,
// including held runs and unfinished exit handlers, appear. Next is opaque.
// Prepared promotions are retained by C6 until publication or abandonment.
type OwnerPage struct {
	Owners []Owner
	Next   string
}
type Owner struct {
	ID       string
	Bindings []sourcecontract.Binding
}

// Keep is idempotent and additive. A run's sealed roots never change. Each
// repository transaction verifies all roots before making any ref visible.
func (c *Cache) Keep(ctx context.Context, owner string, bindings []sourcecontract.Binding) error {
	return c.keepOwner(ctx, owner, bindings)
}
func (c *Cache) keepOwner(ctx context.Context, owner string, bindings []sourcecontract.Binding) error {
	if !ValidOwner(owner) {
		return refuse("source owner")
	}
	for _, b := range bindings {
		if err := b.Validate(); err != nil {
			return err
		}
	}
	for repo, roots := range bindingRoots(bindings) {
		p, err := c.path(repo)
		if err != nil {
			return err
		}
		if err = c.lock(ctx, p); err != nil {
			return err
		}
		err = c.keepRoots(ctx, p, owner, roots)
		if err == nil {
			c.noteKeep(owner)
		}
		c.unlock(p)
		if err != nil {
			return err
		}
	}
	return nil
}
func (c *Cache) keep(ctx context.Context, p, owner string, objects []string) error {
	return c.keepRoots(ctx, p, owner, keepRoots{commits: objects})
}

type keepRoots struct{ commits, objects []string }

func (c *Cache) keepRoots(ctx context.Context, p, owner string, roots keepRoots) error {
	slices.Sort(roots.commits)
	roots.commits = slices.Compact(roots.commits)
	slices.Sort(roots.objects)
	roots.objects = slices.Compact(roots.objects)
	checks := []struct {
		objects []string
		check   func(context.Context, string, string) error
	}{{roots.commits, c.commit}, {roots.objects, c.object}}
	for _, check := range checks {
		for _, oid := range check.objects {
			if err := check.check(ctx, p, oid); err != nil {
				if isAbsent(err) {
					failure := &sourcedelivery.Failure{Code: "gone"}
					return &RefusalError{reason: failure.Error(), remedy: failure.Remedy(), Field: goneField}
				}
				return err
			}
		}
	}
	return c.pinObjects(ctx, p, owner, append(roots.commits, roots.objects...))
}

const goneField = "recorded source"

// sourceGone reports a Keep refused because a recorded root no longer exists.
func sourceGone(err error) bool {
	var refusal *RefusalError
	return errors.As(err, &refusal) && refusal.Field == goneField
}
func (c *Cache) pinObjects(ctx context.Context, p, owner string, objects []string) error {
	slices.Sort(objects)
	objects = slices.Compact(objects)
	var names []string
	var tx strings.Builder
	tx.WriteString("start\n")
	for _, oid := range objects {
		names = append(names, "refs/oberth/keep/"+owner+"/"+oid)
		fmt.Fprintf(&tx, "update refs/oberth/keep/%s/%s %s\n", owner, oid, oid)
	}
	tx.WriteString("prepare\ncommit\n")
	return c.run(ctx, command{dir: p, args: []string{"update-ref", "--stdin"}, input: strings.NewReader(tx.String()),
		refWrites: names})
}

func (c *Cache) keepRefs(ctx context.Context, p string) ([]string, error) {
	data, err := c.capture(
		ctx,
		command{dir: p, args: []string{"for-each-ref", "--format=%(refname)", "refs/oberth/keep/"}},
		16<<20,
	)
	if err != nil {
		return nil, err
	}
	return strings.Fields(string(data)), nil
}
func (c *Cache) release(ctx context.Context, p, owner string) error {
	refs, err := c.keepRefs(ctx, p)
	if err != nil {
		return err
	}
	var names []string
	var tx strings.Builder
	tx.WriteString("start\n")
	for _, ref := range refs {
		if strings.HasPrefix(ref, "refs/oberth/keep/"+owner+"/") {
			names = append(names, ref)
			fmt.Fprintf(&tx, "delete %s\n", ref)
		}
	}
	tx.WriteString("prepare\ncommit\n")
	return c.run(ctx, command{dir: p, args: []string{"update-ref", "--stdin"}, input: strings.NewReader(tx.String()),
		refWrites: names, refDeletes: true})
}

// The run owns its raw ref as well as its commit. An annotated release tag
// therefore survives public-tag deletion without retaining unrelated tags.
func bindingRoots(bindings []sourcecontract.Binding) map[string]keepRoots {
	groups := make(map[string]keepRoots)
	for _, b := range bindings {
		roots := groups[b.Revision.Repository]
		roots.commits = append(roots.commits, b.Revision.Commit)
		for _, r := range []sourcecontract.Ref{b.Export.Ref, b.Export.DefaultBranch} {
			if r.CommitSHA != "" {
				roots.commits = append(roots.commits, r.CommitSHA)
			}
		}
		if b.Export.Ref.ObjectSHA != "" {
			roots.objects = append(roots.objects, b.Export.Ref.ObjectSHA)
		}
		groups[b.Revision.Repository] = roots
	}
	return groups
}
