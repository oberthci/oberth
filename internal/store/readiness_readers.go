package store

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

// OpenReadinessReaders is called once at server startup before sharing Store.
// Its single read-only WAL connection is independent of the writer, dashboard,
// audit, SSH authentication and idle sampler pools. Close owns this pool.
func (s *Store) OpenReadinessReaders(ctx context.Context, probeBudget time.Duration) error {
	if s.readinessDB != nil {
		return nil
	}
	if s.liveReaderDSN == "" || probeBudget <= 0 {
		return fmt.Errorf("%w: readiness readers require live settings and a positive probe budget", ErrInvalidState)
	}
	location, err := url.Parse(s.liveReaderDSN)
	if err != nil {
		return fmt.Errorf("parse readiness reader settings: %w", err)
	}
	query := location.Query()
	pragmas := make([]string, 0, len(query["_pragma"])+1)
	for _, pragma := range query["_pragma"] {
		if !strings.HasPrefix(pragma, "busy_timeout(") {
			pragmas = append(pragmas, pragma)
		}
	}
	// Every new connection must wait at least the sampler budget for SQLite
	// contention, rather than returning a hard SQLITE_BUSY at the default 5s.
	pragmas = append(pragmas, fmt.Sprintf("busy_timeout(%d)", max(s.liveBusyTimeout, probeBudget).Milliseconds()))
	query["_pragma"] = pragmas
	location.RawQuery = query.Encode()
	db, err := s.openLiveReadersWithDSN(ctx, 1, location.String())
	if err != nil {
		return err
	}
	s.readinessDB = db
	return nil
}

func (s *Store) ListReadinessUpstreams(ctx context.Context) ([]model.Upstream, error) {
	if s.readinessDB == nil {
		return nil, fmt.Errorf("%w: readiness readers are not configured", ErrInvalidState)
	}
	return listUpstreams(ctx, s.readinessDB)
}
