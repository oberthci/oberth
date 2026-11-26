package runlog

import (
	"context"
	"errors"
	"io"
	"os"
	"slices"
	"sort"
	"strings"

	"github.com/oberthci/oberth/internal/publicerr"
)

// Containers returns copied physical receipts. State and Reason distinguish
// an interrupted attempt from an exact empty captured span.
func (s *Store) Containers(ctx context.Context, id string) ([]ContainerLog, error) {
	if err := checkID(id); err != nil {
		return nil, err
	}
	if err := s.lock(ctx); err != nil {
		return nil, err
	}
	defer s.unlock()
	saved, err := s.load(id)
	if err != nil {
		return nil, err
	}
	s.availability(id, saved.Containers)
	return slices.Clone(saved.Containers), nil
}
func (s *Store) snapshot(ctx context.Context, id string, request ReadRequest) (*os.File, selected, error) {
	if err := checkID(id); err != nil {
		return nil, selected{}, err
	}
	if err := s.lock(ctx); err != nil {
		return nil, selected{}, err
	}
	defer s.unlock()
	if err := readFacts(request); err != nil {
		return nil, selected{}, err
	}
	saved, err := s.load(id)
	if err != nil {
		return nil, selected{}, err
	}
	s.availability(id, saved.Containers)
	view, err := selection(saved, request)
	if err != nil {
		return nil, view, err
	}
	file, err := s.openBody(id, os.O_RDONLY)
	if errors.Is(err, os.ErrNotExist) && saved.Size == 0 {
		return nil, view, nil
	}
	if err != nil {
		return nil, view, err
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, view, storage(cannotInspectRetainedLogReason, err)
	}
	if unfiltered(request) {
		view.spans = []span{{0, info.Size()}}
	}
	view, err = boundSnapshot(ctx, file, view, snapshotBound{size: info.Size(), before: request.BeforeMarker, after: request.AfterMarker})
	if err != nil {
		_ = file.Close()
		return nil, view, err
	}
	return file, view, nil
}

type snapshotBound struct {
	size          int64
	before, after string
}

func boundSnapshot(ctx context.Context, file *os.File, view selected, b snapshotBound) (selected, error) {
	if err := validSpans(view.spans, b.size); err != nil {
		return view, err
	}
	if b.before == "" && b.after == "" {
		return view, nil
	}
	var err error
	if b.before != "" {
		view.spans, err = beforeMarker(ctx, file, view.spans, b.before)
	}
	if err == nil && b.after != "" {
		view.spans, err = afterMarker(ctx, file, view.spans, b.after)
	}
	view.follow = false
	if len(view.spans) == 0 {
		view.reason = "failed-attempt log pending"
	}
	return view, err
}

func validSpans(spans []span, size int64) error {
	sort.Slice(spans, func(i, j int) bool { return spans[i].start < spans[j].start })
	previous := int64(0)
	for _, part := range spans {
		if part.start < 0 || part.end < part.start || part.end > size || part.start < previous && part.end > part.start {
			return storage(invalidRetainedLogRangeReason, nil)
		}
		previous = max(previous, part.end)
	}
	return nil
}

// Read applies a bounded plain-text filter to a snapshot of selected spans.
func (s *Store) Read(ctx context.Context, id string, request ReadRequest) ([]byte, Meta, error) {
	file, view, err := s.snapshot(ctx, id, request)
	if err != nil {
		return nil, Meta{Reason: publicerr.Classify(err).Reason(), Follow: view.follow, Copying: view.copying, Waiting: view.waiting}, err
	}
	var source io.ReaderAt = strings.NewReader("")
	if file != nil {
		defer func() { _ = file.Close() }()
		source = file
	}
	body, meta, err := scan(ctx, source, view.spans, request.Filter)
	meta.Reason, meta.Detail, meta.Follow = view.reason, view.detail, view.follow
	meta.Copying, meta.Waiting = view.copying, view.waiting
	meta.Truncated = meta.Truncated || view.truncated
	if meta.Bytes == 0 && meta.Reason == "" && unfiltered(request) {
		meta.Reason = noOutputReason
	}
	return body, meta, err
}

// OpenStream opens only stored spans, never a native or network log stream.
// The caller closes it. Reads observe cancellation without changing capture state.
func (s *Store) OpenStream(ctx context.Context, id string, request ReadRequest) (io.ReadCloser, error) {
	file, view, err := s.snapshot(ctx, id, request)
	if err != nil {
		return nil, err
	}
	readers := make([]io.Reader, 0, len(view.spans))
	for _, part := range view.spans {
		readers = append(readers, io.NewSectionReader(file, part.start, part.end-part.start))
	}
	return &storedStream{ctx: ctx, file: file, reader: io.MultiReader(readers...)}, nil
}

type storedStream struct {
	ctx    context.Context
	file   *os.File
	reader io.Reader
}

func (r *storedStream) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, storage(logOperationCanceledReason, err)
	}
	return r.reader.Read(p)
}
func (r *storedStream) Close() error {
	if r.file == nil {
		return nil
	}
	return r.file.Close()
}
