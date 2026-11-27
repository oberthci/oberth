package gitcache

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"

	"github.com/oberthci/oberth/internal/sourcecontract"
	"github.com/oberthci/oberth/internal/sourcedelivery"
)

// Reservations, builders, completed files and reader-held files share one
// bound. A reservation owns its build until completion or its last waiter
// leaves. Only idle completed entries may be evicted.
type stagedArchives struct {
	mu       sync.Mutex
	entries  map[[sha256.Size]byte]*stagedArchive
	changed  chan struct{}
	sequence uint64
	capacity int
	closed   bool
	workers  sync.WaitGroup
}

type stagedArchive struct {
	key                         [sha256.Size]byte
	observation                 *sourcedelivery.Observation
	dir                         string
	bytes                       int64
	readers, waiters            int
	used                        uint64
	done                        chan struct{}
	building, abandoned, waited bool
	err                         error
	cancel                      context.CancelFunc
	release                     func()
}

func newStagedArchives(capacity int) *stagedArchives {
	return &stagedArchives{entries: make(map[[sha256.Size]byte]*stagedArchive), changed: make(chan struct{}), capacity: capacity}
}

func archiveIdentity(b sourcecontract.Binding) [sha256.Size]byte {
	// Encoding includes the authorization digest and entire checkout inventory.
	data, _ := json.Marshal(b)
	return sha256.Sum256(data)
}

func (s *stagedArchives) notify() { close(s.changed); s.changed = make(chan struct{}) }

// reserve registers waiter ownership before any export permit is acquired.
func (s *stagedArchives) reserve(ctx context.Context, key [sha256.Size]byte) (*stagedArchive, bool, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	waited := false
	for {
		if err := ctx.Err(); err != nil {
			return nil, false, waited, context.Cause(ctx)
		}
		if s.closed {
			return nil, false, waited, context.Canceled
		}
		if e := s.join(key); e != nil {
			return e, false, waited || e.building, nil
		}
		if s.entries[key] == nil && len(s.entries) < s.capacity {
			e := &stagedArchive{key: key, done: make(chan struct{}), building: true, waiters: 1}
			s.entries[key] = e
			s.workers.Add(1)
			return e, true, waited, nil
		}
		if evicted, err := s.evictIdle(); err != nil {
			return nil, false, waited, err
		} else if evicted {
			continue
		}
		changed := s.changed
		waited = true
		s.mu.Unlock()
		end := sourcedelivery.Measure(ctx, sourcedelivery.Archive, true)
		select {
		case <-ctx.Done():
		case <-changed:
		}
		end()
		s.mu.Lock()
	}
}

// Caller holds mu; joining preserves ownership through build completion.
func (s *stagedArchives) join(key [sha256.Size]byte) *stagedArchive {
	e := s.entries[key]
	if e != nil && !e.abandoned {
		e.waiters++
		return e
	}
	return nil
}

func (s *stagedArchives) start(e *stagedArchive, cancel context.CancelFunc, release func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e.cancel, e.release = cancel, release
	if s.closed || e.abandoned {
		cancel()
	}
}

func (s *stagedArchives) complete(e *stagedArchive, dir string, stage sourcedelivery.Staged, err error) {
	if stage.Body != nil {
		err = errors.Join(err, stage.Body.Close())
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e.cancel()
	e.dir, e.bytes, e.waited = dir, stage.Bytes, stage.Waited
	if err != nil || e.waiters == 0 || s.closed {
		cleanup := os.RemoveAll(dir)
		err = errors.Join(err, cleanup)
		if err == nil {
			err = context.Canceled
		}
		if cleanup == nil {
			delete(s.entries, e.key)
			e.release()
		} else {
			e.abandoned = true
		}
	}
	e.building, e.err = false, err
	close(e.done)
	s.notify()
	s.workers.Done()
}

func (s *stagedArchives) leave(e *stagedArchive) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e.waiters--
	if e.waiters == 0 && e.building {
		e.abandoned = true
		if e.cancel != nil {
			e.cancel()
		}
	}
	s.notify()
}

func (s *stagedArchives) open(e *stagedArchive) (sourcedelivery.Staged, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e.err != nil {
		return sourcedelivery.Staged{}, e.err
	}
	if s.closed {
		return sourcedelivery.Staged{}, context.Canceled
	}
	f, err := os.Open(filepath.Join(e.dir, "source.tgz"))
	if err != nil {
		return sourcedelivery.Staged{}, err
	}
	s.sequence++
	e.used, e.readers = s.sequence, e.readers+1
	return sourcedelivery.Staged{Body: &stagedReader{File: f, cache: s, entry: e}, Bytes: e.bytes}, nil
}

// Caller holds mu. Waiters keep a completed entry alive until they open it.
func (s *stagedArchives) evictIdle() (bool, error) {
	var oldest *stagedArchive
	var key [sha256.Size]byte
	for k, e := range s.entries {
		if !e.building && e.readers == 0 && e.waiters == 0 && (oldest == nil || e.used < oldest.used) {
			key, oldest = k, e
		}
	}
	if oldest == nil {
		return false, nil
	}
	if err := os.RemoveAll(oldest.dir); err != nil {
		return false, err
	}
	delete(s.entries, key)
	oldest.release()
	return true, nil
}

func (s *stagedArchives) prune(live map[[sha256.Size]byte]bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var failures error
	for k, e := range s.entries {
		if e.building || e.readers != 0 || e.waiters != 0 || live[k] {
			continue
		}
		if err := os.RemoveAll(e.dir); err != nil {
			failures = errors.Join(failures, err)
			continue
		}
		delete(s.entries, k)
		e.release()
	}
	s.notify()
	return failures
}

func (s *stagedArchives) close() {
	s.mu.Lock()
	s.closed = true
	for _, e := range s.entries {
		if e.building {
			e.abandoned = true
			if e.cancel != nil {
				e.cancel()
			}
		}
	}
	s.notify()
	s.mu.Unlock()
	s.workers.Wait()
}

type stagedReader struct {
	*os.File
	cache *stagedArchives
	entry *stagedArchive
	once  sync.Once
	err   error
}

func (r *stagedReader) Close() error {
	r.once.Do(func() {
		r.err = r.File.Close()
		r.cache.mu.Lock()
		defer r.cache.mu.Unlock()
		r.entry.readers--
		r.cache.notify()
	})
	return r.err
}

func (s *stagedArchives) buildObservation(e *stagedArchive, observation *sourcedelivery.Observation) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e.observation = observation
}

func (s *stagedArchives) observation(e *stagedArchive) *sourcedelivery.Observation {
	s.mu.Lock()
	defer s.mu.Unlock()
	return e.observation
}
