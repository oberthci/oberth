package runlog

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"sync"

	"github.com/oberthci/oberth/internal/publicerr"
)

// Store is the single owner of a log root. Close follows cancellation and join
// of its users. Source reads never hold its cancelable append/index gate.
type Store struct {
	root      *os.Root
	directory string
	limit     int64
	gate      chan struct{}
	active    sync.Map
	following sync.Map
}

func Open(ctx context.Context, directory string, limit int64) (*Store, error) {
	if err := ctx.Err(); err != nil {
		return nil, storage(logOperationCanceledReason, err)
	}
	if limit <= 0 {
		return nil, publicerr.New(publicerr.InvalidInput, runLogLimitMustBePositiveReason, runLimitRemedy, nil)
	}
	if err := os.MkdirAll(directory, 0700); err != nil {
		return nil, storage(cannotCreateLogRootReason, err)
	}
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() {
		return nil, storage(logRootMustBeARegularDirectoryReason, err)
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return nil, storage(cannotOpenLogRootReason, err)
	}
	return &Store{root: root, directory: directory, limit: limit, gate: make(chan struct{}, 1)}, nil
}
func (s *Store) Close() error { return s.root.Close() }
func (s *Store) lock(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return storage(logOperationCanceledReason, err)
	}
	select {
	case s.gate <- struct{}{}:
		if err := ctx.Err(); err != nil {
			s.unlock()
			return storage(logOperationCanceledReason, err)
		}
		return nil
	case <-ctx.Done():
		return storage(logOperationCanceledReason, ctx.Err())
	}
}
func (s *Store) unlock() { <-s.gate }
func validID(id string) bool {
	if id == "" || len(id) > 80 {
		return false
	}
	return strings.IndexFunc(id, func(r rune) bool {
		return !strings.ContainsRune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_", r)
	}) < 0
}
func checkID(id string) error {
	if !validID(id) {
		return publicerr.New(publicerr.InvalidInput, invalidRunIDReason, runIDRemedy, nil)
	}
	return nil
}
func storage(reason string, cause error) error {
	remedy := storageRemedy
	if errors.Is(cause, context.Canceled) || errors.Is(cause, context.DeadlineExceeded) {
		remedy = canceledRemedy
	}
	return publicerr.New(publicerr.Unavailable, reason, remedy, cause)
}
func (s *Store) openBody(id string, flags int) (*os.File, error) {
	name := id + ".log"
	if info, err := s.root.Lstat(name); err == nil && !info.Mode().IsRegular() {
		return nil, storage(logEvidenceMustBeARegularFileReason, nil)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, storage(cannotInspectLogEvidenceReason, err)
	}
	file, err := s.root.OpenFile(name, flags, 0600)
	if err != nil {
		return nil, storage(cannotOpenRetainedLogReason, err)
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = file.Close()
		return nil, storage(logEvidenceMustBeARegularFileReason, err)
	}
	return file, nil
}

// Append adds ordinary output under the same aggregate cap as container copies.
// A full log keeps its explanation in the index even if no text fits the body.
func (s *Store) Append(ctx context.Context, id string, body []byte) error {
	if err := checkID(id); err != nil {
		return err
	}
	if err := s.lock(ctx); err != nil {
		return err
	}
	defer s.unlock()
	saved, err := s.load(id)
	if err != nil {
		return err
	}
	file, err := s.openBody(id, os.O_CREATE|os.O_RDWR)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	size, err := appendPosition(file, saved.Size)
	if err != nil {
		return storage(cannotFindLogAppendPositionReason, err)
	}
	kept := min(int64(len(body)), max(0, s.limit-size))
	if kept < int64(len(body)) {
		saved.Reason, saved.Detail = limitReason, limitDetail
	}
	if _, err = file.Write(body[:kept]); err != nil {
		return storage(cannotAppendLogBytesReason, err)
	}
	if err = file.Sync(); err != nil {
		return storage(cannotSyncLogBytesReason, err)
	}
	saved.Size = size + kept
	return s.save(id, saved)
}

// appendPosition discards only unpublished bytes; committed spans stay intact.
func appendPosition(file *os.File, committed int64) (int64, error) {
	info, err := file.Stat()
	if err != nil {
		return 0, err
	}
	if info.Size() < committed {
		return 0, storage(retainedLogBytesAreUnavailableReason, nil)
	}
	if err = file.Truncate(committed); err != nil {
		return 0, err
	}
	return file.Seek(0, io.SeekEnd)
}
