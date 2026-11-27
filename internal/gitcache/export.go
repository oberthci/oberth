package gitcache

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"os"
	"path/filepath"

	"github.com/oberthci/oberth/internal/gitport"
	"github.com/oberthci/oberth/internal/sourcecontract"
	"github.com/oberthci/oberth/internal/sourcedelivery"
)

const (
	MainBytes        int64 = 512 << 20
	PinBytes         int64 = 128 << 20
	PackBytes        int64 = 512 << 20
	MetadataBytes    int64 = 512 << 20
	MainArchiveBytes int64 = sourcedelivery.MaxMainArchiveBytes
	PinArchiveBytes  int64 = sourcedelivery.MaxPinArchiveBytes
	PinFiles               = 4096
)

// Stage reuses a sealed binding's archive with an independent reader per load.
// Authorization is always done by the caller. Repository recovery and recorded
// object availability are checked even on a hit; no locks span network writes.
func (c *Cache) Stage(ctx context.Context, b sourcecontract.Binding) (sourcedelivery.Staged, error) {
	if err := b.Validate(); err != nil {
		return sourcedelivery.Staged{}, &sourcedelivery.Failure{Code: "forbidden"}
	}
	ctx, cancel := context.WithTimeout(ctx, c.budgets.local)
	defer cancel()
	key := archiveIdentity(b)
	e, leader, waited, err := c.staged.reserve(ctx, key)
	if err != nil {
		return sourcedelivery.Staged{}, err
	}
	defer c.staged.leave(e)
	inherit := func() {
		if leader || waited {
			sourcedelivery.InheritWait(ctx, c.staged.observation(e))
		}
	}
	if leader {
		c.startArchive(e, b) //nolint:contextcheck // The cache owns this shared build; the last waiter or shutdown cancels it.
	} else if waited {
		sourcedelivery.Joined(ctx)
	}
	end := sourcedelivery.Measure(ctx, sourcedelivery.Archive, waited)
	select {
	case <-ctx.Done():
		end()
		inherit()
		return sourcedelivery.Staged{}, context.Cause(ctx)
	case <-e.done:
		end()
	}
	inherit()
	if err := ctx.Err(); err != nil {
		return sourcedelivery.Staged{}, context.Cause(ctx)
	}
	stage, err := c.staged.open(e)
	stage.Waited = waited || (leader && e.waited)
	return c.checkedStage(ctx, b, stage, err)
}

func (c *Cache) startArchive(e *stagedArchive, b sourcecontract.Binding) {
	ctx, cancel := context.WithTimeout(c.ctx, c.budgets.local)
	ctx, observation := sourcedelivery.Observe(ctx, "job")
	c.staged.buildObservation(e, observation)
	c.staged.start(e, cancel, c.config.Permits.Enter(sourcedelivery.Archive))
	go func() {
		dir, stage, err := c.buildArchive(ctx, b, e)
		summary := finishObservation(observation, err)
		summary.Leaders = 1
		if !summary.Blocked {
			summary.Phase = sourcedelivery.Export
		}
		if c.config.Observe != nil {
			c.config.Observe(summary)
		}
		c.staged.complete(e, dir, stage, stageFailure(b, err))
	}()
}

func (c *Cache) buildArchive(ctx context.Context, b sourcecontract.Binding, e *stagedArchive) (string, sourcedelivery.Staged, error) {
	waited := false
	if err := ctx.Err(); err != nil {
		return "", sourcedelivery.Staged{}, context.Cause(ctx)
	}
	select {
	case c.export <- struct{}{}:
	default:
		waited = true
		end := sourcedelivery.Measure(ctx, sourcedelivery.Export, true)
		err := acquireContext(ctx, c.export)
		end()
		if err != nil {
			return "", sourcedelivery.Staged{}, err
		}
	}
	leave := c.config.Permits.Enter(sourcedelivery.Export)
	release := func() { leave(); <-c.export }
	defer func() { release() }()
	dir, err := os.MkdirTemp(c.config.Scratch, "oberth-export-")
	if err != nil {
		return "", sourcedelivery.Staged{}, err
	}
	stage, err := c.build(ctx, dir, b)
	stage.Waited = stage.Waited || waited
	if err != nil {
		cleanup := os.RemoveAll(dir)
		err = errors.Join(err, cleanup)
		if cleanup != nil {
			// The existing archive owner retains E along with A until its
			// failed workspace can be removed by eviction, pruning or close.
			c.staged.retainExport(e, release)
			release = func() {}
		}
	}
	return dir, stage, err
}
func (c *Cache) build(ctx context.Context, dir string, b sourcecontract.Binding) (stage sourcedelivery.Staged, result error) {
	waited := false
	defer func() { stage.Waited = stage.Waited || waited }()
	defer sourcedelivery.Measure(ctx, sourcedelivery.Export, false)()
	p, err := c.path(b.Revision.Repository)
	if err != nil {
		return sourcedelivery.Staged{}, err
	}
	if waited, err = c.lockReadWait(ctx, p); err != nil {
		return sourcedelivery.Staged{}, err
	}
	defer c.unlock(p)
	if err = c.commit(ctx, p, b.Revision.Commit); err != nil {
		return sourcedelivery.Staged{}, err
	}
	limit, archiveLimit := MainBytes, MainArchiveBytes
	if b.Descriptor.Kind == sourcecontract.Pinned {
		limit, archiveLimit = PinBytes, PinArchiveBytes
	}
	archive, err := os.OpenFile(filepath.Join(dir, "source.tgz"), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return sourcedelivery.Staged{}, err
	}
	defer func() { _ = archive.Close() }()
	gz := gzip.NewWriter(&limitedWriter{writer: archive, left: archiveLimit})
	tw := tar.NewWriter(gz)
	writer := archiveWriter{cache: c, tar: tw, store: p, scratch: dir, binding: b, limit: limit}
	err = writer.tree(ctx)
	if err == nil && b.Descriptor.Kind == sourcecontract.Main {
		err = writer.history(ctx)
	}
	err = errors.Join(err, tw.Close(), gz.Close())
	if err != nil {
		return sourcedelivery.Staged{}, err
	}
	if _, err = archive.Seek(0, 0); err != nil {
		return sourcedelivery.Staged{}, err
	}
	f, err := os.Open(archive.Name())
	if err != nil {
		return sourcedelivery.Staged{}, err
	}
	return sourcedelivery.Staged{Body: f, Bytes: writer.total}, nil
}

func (c *Cache) checkedStage(ctx context.Context, b sourcecontract.Binding, stage sourcedelivery.Staged, err error) (sourcedelivery.Staged, error) {
	if err == nil {
		var waited bool
		waited, err = c.checkStagedObjects(ctx, b)
		stage.Waited = stage.Waited || waited
	}
	if err != nil {
		if stage.Body != nil {
			err = errors.Join(err, stage.Body.Close())
		}
		return sourcedelivery.Staged{}, stageFailure(b, err)
	}
	return stage, nil
}

func (c *Cache) checkStagedObjects(ctx context.Context, b sourcecontract.Binding) (bool, error) {
	p, err := c.path(b.Revision.Repository)
	if err != nil {
		return false, err
	}
	waited, err := c.lockReadWait(ctx, p)
	if err != nil {
		return waited, err
	}
	defer c.unlock(p)
	if err = c.commit(ctx, p, b.Revision.Commit); err != nil {
		return waited, err
	}
	refs := append([]sourcecontract.Ref{b.Export.Ref, b.Export.DefaultBranch}, b.Export.Tags...)
	return waited, c.checkArchiveObjects(ctx, p, refs)
}

func cleanExports(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if len(e.Name()) > len("oberth-export-") && e.Name()[:len("oberth-export-")] == "oberth-export-" {
			if err = os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}

func stageFailure(b sourcecontract.Binding, err error) error {
	var limit *LimitError
	var absent *gitport.AbsentError
	var moved *tagMovedError
	switch {
	case errors.As(err, &limit):
		n := limit.Limit
		if n == 0 {
			n = MainBytes
			if b.Descriptor.Kind == sourcecontract.Pinned {
				n = PinBytes
			}
		}
		return &sourcedelivery.Failure{Code: "too_large", Limit: n, Unit: limit.Unit}
	case errors.As(err, &moved):
		return &sourcedelivery.Failure{Code: "gone", Message: reasonTagMoved}
	case errors.As(err, &absent):
		return &sourcedelivery.Failure{Code: "gone"}
	}
	var refusal *gitport.AuthorRefusalError
	var sourceRefusal *RefusalError
	var invalid *sourcecontract.InvalidError
	if errors.As(err, &refusal) || errors.As(err, &sourceRefusal) || errors.As(err, &invalid) {
		return &sourcedelivery.Failure{Code: "forbidden"}
	}
	return err
}

type tagMovedError struct{ error }

func (e *tagMovedError) Unwrap() error { return e.error }
