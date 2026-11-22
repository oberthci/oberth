package gitcache

import (
	"context"
	"errors"
	"os"
)

// fetchHeadObject brings the forge's head object o into the store at p
// through a scratch repository and imports it only while r is still the
// repository's registered upstream: a transfer that outlived an address or
// catalogue ID change imports nothing, as a refresh and a first clone do.
func (c *Cache) fetchHeadObject(ctx context.Context, p string, r Remote, o fetchedObject) (result error) {
	dir, err := os.MkdirTemp(c.config.Scratch, "oberth-export-object-")
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, os.RemoveAll(dir)) }()
	if err = c.scratchRepository(ctx, p, dir); err != nil {
		return err
	}
	o.into = "refs/oberth/object"
	if err = c.fetchObject(ctx, dir, r, o); err != nil {
		return err
	}
	if err = c.sameUpstream(ctx, r.repository, r); err != nil {
		return err
	}
	return c.run(ctx, command{dir: p, args: []string{"fetch", "--no-auto-maintenance", "--no-write-fetch-head", "--no-tags", "--", dir, o.oid}})
}

// forgeJobKey is the job key of a forge read of the repository at p: one job
// per forge identity and default branch, so a caller whose registration
// changed never takes another forge's answer.
func forgeJobKey(p, kind string, r Remote, oid string) string {
	return p + "/" + kind + "\x00" + forgeIdentity(r) + "\x00" + r.DefaultBranch + "\x00" + oid
}
