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
	if stage, found, err := c.staged.open(key); found {
		return c.checkedStage(ctx, b, stage, err)
	}
	waited := false
	select {
	case c.export <- struct{}{}:
	default:
		waited = true
		end := sourcedelivery.Measure(ctx, sourcedelivery.Export, true)
		select {
		case c.export <- struct{}{}:
			end()
		case <-ctx.Done():
			end()
			return sourcedelivery.Staged{}, context.Cause(ctx)
		}
	}
	defer func() { <-c.export }()
	defer c.config.Permits.Enter(sourcedelivery.Export)()
	if stage, found, err := c.staged.open(key); found {
		sourcedelivery.Joined(ctx)
		stage.Waited = waited
		return c.checkedStage(ctx, b, stage, err)
	}
	roomWaited, err := c.staged.room(ctx)
	if err != nil {
		return sourcedelivery.Staged{}, err
	}
	dir, err := os.MkdirTemp(c.config.Scratch, "oberth-export-")
	if err != nil {
		return sourcedelivery.Staged{}, err
	}
	stage, err := c.build(ctx, dir, b)
	if err != nil {
		_ = os.RemoveAll(dir)
		return stage, stageFailure(b, err)
	}
	stage.Waited = waited || roomWaited
	return c.staged.put(key, dir, stage), nil
}
func (c *Cache) build(ctx context.Context, dir string, b sourcecontract.Binding) (sourcedelivery.Staged, error) {
	defer sourcedelivery.Measure(ctx, sourcedelivery.Export, false)()
	p, err := c.path(b.Revision.Repository)
	if err != nil {
		return sourcedelivery.Staged{}, err
	}
	if err = c.lockRead(ctx, p); err != nil {
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
		err = c.checkStagedObjects(ctx, b)
	}
	if err != nil {
		if stage.Body != nil {
			err = errors.Join(err, stage.Body.Close())
		}
		return sourcedelivery.Staged{}, stageFailure(b, err)
	}
	return stage, nil
}

func (c *Cache) checkStagedObjects(ctx context.Context, b sourcecontract.Binding) error {
	p, err := c.path(b.Revision.Repository)
	if err != nil {
		return err
	}
	if err = c.lockRead(ctx, p); err != nil {
		return err
	}
	defer c.unlock(p)
	if err = c.commit(ctx, p, b.Revision.Commit); err != nil {
		return err
	}
	refs := append([]sourcecontract.Ref{b.Export.Ref, b.Export.DefaultBranch}, b.Export.Tags...)
	return c.checkArchiveObjects(ctx, p, refs)
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
