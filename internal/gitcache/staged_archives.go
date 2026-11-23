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

// At most two archives (including files held by readers) share the chart's
// scratch allowance. The compression slot serializes misses. Idle entries are
// evicted in use order; a full cache with active readers waits for a Close.
type stagedArchives struct {
	mu       sync.Mutex
	entries  map[[sha256.Size]byte]*stagedArchive
	changed  chan struct{}
	sequence uint64
}

type stagedArchive struct {
	dir     string
	bytes   int64
	readers int
	used    uint64
}

func newStagedArchives() *stagedArchives {
	return &stagedArchives{entries: make(map[[sha256.Size]byte]*stagedArchive), changed: make(chan struct{})}
}

func archiveIdentity(b sourcecontract.Binding) [sha256.Size]byte {
	// Binding's encoding sorts its recorded tags and includes repository identity,
	// authorization, revision and the entire admission-time checkout inventory.
	data, _ := json.Marshal(b)
	return sha256.Sum256(data)
}

func (s *stagedArchives) open(key [sha256.Size]byte) (sourcedelivery.Staged, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.entries[key]
	if e == nil {
		return sourcedelivery.Staged{}, false, nil
	}
	f, err := os.Open(filepath.Join(e.dir, "source.tgz"))
	if err != nil {
		return sourcedelivery.Staged{}, true, err
	}
	s.sequence++
	e.used, e.readers = s.sequence, e.readers+1
	return sourcedelivery.Staged{Body: &stagedReader{File: f, cache: s, entry: e}, Bytes: e.bytes}, true, nil
}

func (s *stagedArchives) room(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for len(s.entries) >= 2 {
		if evicted, err := s.evictIdle(); err != nil {
			return err
		} else if evicted {
			continue
		}
		changed := s.changed
		s.mu.Unlock()
		select {
		case <-ctx.Done():
			s.mu.Lock()
			return context.Cause(ctx)
		case <-changed:
			s.mu.Lock()
		}
	}
	return ctx.Err()
}

// Caller holds mu and the compression slot, so only readers can free space.
func (s *stagedArchives) evictIdle() (bool, error) {
	var oldest *stagedArchive
	var key [sha256.Size]byte
	for k, e := range s.entries {
		if e.readers == 0 && (oldest == nil || e.used < oldest.used) {
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
	return true, nil
}

func (s *stagedArchives) put(key [sha256.Size]byte, dir string, stage sourcedelivery.Staged) sourcedelivery.Staged {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sequence++
	e := &stagedArchive{dir: dir, bytes: stage.Bytes, readers: 1, used: s.sequence}
	s.entries[key] = e
	stage.Body = &stagedReader{File: stage.Body.(*os.File), cache: s, entry: e}
	return stage
}

func (s *stagedArchives) prune(live map[[sha256.Size]byte]bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var failures error
	for k, e := range s.entries {
		if e.readers != 0 || live[k] {
			continue
		}
		if err := os.RemoveAll(e.dir); err != nil {
			failures = errors.Join(failures, err)
			continue
		}
		delete(s.entries, k)
	}
	return failures
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
		close(r.cache.changed)
		r.cache.changed = make(chan struct{})
	})
	return r.err
}
