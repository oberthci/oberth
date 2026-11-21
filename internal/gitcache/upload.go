package gitcache

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"github.com/oberthci/oberth/internal/gitport"
	"github.com/oberthci/oberth/internal/sourcecontract"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// ServiceIO brackets only native client transport, so preparation and durable
// finalization do not consume an SSH stream's inactivity allowance.
type ServiceIO struct {
	Input       io.ReadCloser
	Output      io.Writer
	Diagnostics io.Writer
	Begin, End  func()
}

// Upload snapshots public refs and borrows retained objects through Git's local
// alternate mechanism. Slow clients hold their own refs, never the cache lock
// or a second on-disk copy of repository history.
func (c *Cache) Upload(ctx context.Context, repo string, streams ServiceIO) error {
	if streams.Input == nil {
		return refuse("Git transport")
	}
	if err := c.RefreshRead(ctx, repo, streams.Diagnostics); err != nil {
		return err
	}
	p, err := c.path(repo)
	if err != nil {
		return err
	}
	dir, err := os.MkdirTemp(c.config.Scratch, "oberth-export-upload-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	owner, err := c.uploadSnapshot(ctx, p, dir)
	if err != nil {
		return err
	}
	err = c.uploadTransport(ctx, dir, streams)
	return errors.Join(err, c.finishUpload(ctx, p, owner))
}
func (c *Cache) uploadTransport(ctx context.Context, dir string, streams ServiceIO) error {
	if streams.Begin != nil {
		streams.Begin()
	}
	defer func() {
		if streams.End != nil {
			streams.End()
		}
	}()
	stop := context.AfterFunc(ctx, func() { _ = streams.Input.Close() })
	defer stop()
	return c.run(ctx, command{dir: dir, args: []string{"upload-pack", "."}, input: streams.Input, output: streams.Output})
}
func (c *Cache) uploadSnapshot(ctx context.Context, p, dir string) (string, error) {
	if err := c.lockRead(ctx, p); err != nil {
		return "", err
	}
	defer c.unlock(p)
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		return "", err
	}
	owner := "upload-" + hex.EncodeToString(id)
	refs, err := c.list(ctx, p, "")
	if err != nil {
		return "", err
	}
	objects := make([]string, 0, len(refs))
	for _, ref := range refs {
		objects = append(objects, ref.ObjectSHA)
	}
	if err = c.pinObjects(ctx, p, owner, objects); err != nil {
		return "", err
	}
	c.setActive(owner, true)
	if err = c.initializeUpload(ctx, p, dir, refs); err != nil {
		c.setActive(owner, false)
		return "", err
	}
	return owner, nil
}
func (c *Cache) finishUpload(ctx context.Context, p, owner string) error {
	c.setActive(owner, false)
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := c.lockRead(ctx, p); err != nil {
		return err
	}
	defer c.unlock(p)
	return c.release(ctx, p, owner)
}
func (c *Cache) setActive(owner string, live bool) {
	c.activeMu.Lock()
	defer c.activeMu.Unlock()
	if live {
		c.active[owner] = true
	} else {
		delete(c.active, owner)
	}
}
func (c *Cache) isActive(owner string) bool {
	c.activeMu.Lock()
	defer c.activeMu.Unlock()
	return c.active[owner]
}

func (c *Cache) initializeUpload(ctx context.Context, p, dir string, refs []gitport.Ref) error {
	format, err := c.capture(ctx, command{dir: p, args: []string{"rev-parse", "--show-object-format"}}, 64)
	if err != nil {
		return err
	}
	if err = c.run(
		ctx,
		command{
			dir:  dir,
			args: []string{"init", "--bare", "--object-format=" + string(format), "."},
		},
	); err != nil {
		return err
	}
	head, err := os.ReadFile(filepath.Join(p, "HEAD"))
	if err != nil {
		return err
	}
	for name, data := range map[string][]byte{
		"HEAD": head, "config": repositoryConfig(string(format), true),
		"objects/info/alternates": []byte(strconv.Quote(filepath.Join(p, "objects")) + "\n"),
	} {
		if err = os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
			return err
		}
	}
	snapshot := make([]sourcecontract.Ref, len(refs))
	for i, ref := range refs {
		snapshot[i] = sourcecontract.Ref(ref)
	}
	return c.snapshotRefs(ctx, dir, snapshot)
}
