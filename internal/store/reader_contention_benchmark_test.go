package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"modernc.org/sqlite"

	"github.com/oberthci/oberth/internal/model"
)

// Measure the actual writer reservation, excluding connection acquisition and
// BEGIN wait. This driver exists only in benchmarks; no production timing or
// SQL interception is installed. Keep a bounded tail for the percentile.
type writerHoldSamples struct {
	mu    sync.Mutex
	count int
	max   time.Duration
	tail  [1024]time.Duration
}

func (s *writerHoldSamples) record(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tail[s.count%len(s.tail)] = d
	s.count++
	s.max = max(s.max, d)
}

type writerHoldDriver struct {
	driver.Driver
	samples *writerHoldSamples
}

type writerHoldConn struct {
	driver.Conn
	driver.ConnPrepareContext
	driver.ExecerContext
	driver.QueryerContext
	driver.SessionResetter
	driver.Validator
	begin   driver.ConnBeginTx
	samples *writerHoldSamples
}

func (d writerHoldDriver) Open(name string) (driver.Conn, error) {
	c, err := d.Driver.Open(name)
	if err != nil {
		return nil, err
	}
	return &writerHoldConn{Conn: c, ConnPrepareContext: c.(driver.ConnPrepareContext), ExecerContext: c.(driver.ExecerContext), QueryerContext: c.(driver.QueryerContext), SessionResetter: c.(driver.SessionResetter), Validator: c.(driver.Validator), begin: c.(driver.ConnBeginTx), samples: d.samples}, nil
}

type writerHoldContextKey struct{}

func (c *writerHoldConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	tx, err := c.begin.BeginTx(ctx, opts)
	if err != nil || ctx.Value(writerHoldContextKey{}) == nil {
		return tx, err
	}
	return &writerHoldTx{Tx: tx, start: time.Now(), samples: c.samples}, nil
}

type writerHoldTx struct {
	driver.Tx
	start   time.Time
	samples *writerHoldSamples
	once    sync.Once
}

func (tx *writerHoldTx) Commit() error {
	err := tx.Tx.Commit()
	tx.once.Do(func() { tx.samples.record(time.Since(tx.start)) })
	return err
}

func (tx *writerHoldTx) Rollback() error {
	err := tx.Tx.Rollback()
	tx.once.Do(func() { tx.samples.record(time.Since(tx.start)) })
	return err
}

var writerHoldDriverID atomic.Uint64

func benchmarkMeasuredWriter(b *testing.B, s *Store) *writerHoldSamples {
	b.Helper()
	if err := s.db.Close(); err != nil {
		b.Fatal(err)
	}
	samples := &writerHoldSamples{}
	name := fmt.Sprintf("writer-hold-%d", writerHoldDriverID.Add(1))
	sql.Register(name, writerHoldDriver{Driver: &sqlite.Driver{}, samples: samples})
	dsn, err := immediateTransactionDSN(s.livePath, defaultBusyTimeout)
	if err != nil {
		b.Fatal(err)
	}
	db, err := sql.Open(name, dsn)
	if err != nil {
		b.Fatal(err)
	}
	s.db = db // Before starting reader goroutines; Store.Close owns this handle.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	for _, pragma := range []string{`PRAGMA foreign_keys=ON`, `PRAGMA synchronous=FULL`} {
		if _, err := db.ExecContext(b.Context(), pragma); err != nil {
			b.Fatal(err)
		}
	}
	return samples
}

func BenchmarkWriterHoldUnderAPIReads(b *testing.B) {
	for _, name := range []string{"timings64", "timings256", "timings2048", "priority-config", "audit-append"} {
		b.Run(name, func(b *testing.B) {
			tasks := 64
			if name == "timings256" {
				tasks = 256
			}
			if name == "timings2048" {
				tasks = 2048
			}
			s, values := timingBatchFixture(b, tasks)
			samples := benchmarkMeasuredWriter(b, s)
			ctx, cancel := context.WithCancel(b.Context())
			var wg sync.WaitGroup
			var reads atomic.Uint64
			errs := make(chan error, 1)
			started := make(chan struct{}, 4)
			for range 4 {
				wg.Go(func() {
					ready := false
					for ctx.Err() == nil {
						_, err := s.ListRepositories(ctx)
						if err == nil {
							_, err = s.ListRecentRuns(ctx, model.RunListFilter{})
						}
						if err == nil {
							_, err = s.QueuePriorityPatterns(ctx)
						}
						if err == nil {
							_, err = s.NextQueuedRun(ctx)
						}
						if err != nil {
							if ctx.Err() == nil {
								select {
								case errs <- err:
								default:
								}
							}
							if !ready {
								started <- struct{}{}
							}
							return
						}
						reads.Add(4)
						if !ready {
							started <- struct{}{}
							ready = true
						}
					}
				})
			}
			defer func() { cancel(); wg.Wait() }()
			for range 4 {
				<-started
			}
			writeCtx := context.WithValue(b.Context(), writerHoldContextKey{}, true)
			b.ResetTimer()
			for range b.N {
				var err error
				switch name {
				case "priority-config":
					_, err = s.SetQueuePriorityPatterns(writeCtx, "benchmark", []string{"rc/*"})
				case "audit-append":
					_, err = s.AppendAuditAction(writeCtx, model.AuditActionSpec{Actor: "benchmark", Action: "benchmark", ResourceType: "benchmark", ResourceID: "benchmark", Details: "{}"})
				default:
					b.StopTimer()
					_, err = s.db.ExecContext(b.Context(), `DELETE FROM step_timings WHERE run_id=?`, values[0].RunID)
					b.StartTimer()
					if err == nil {
						err = s.RecordStepTimings(writeCtx, values)
					}
				}
				if err != nil {
					b.Fatal(err)
				}
			}
			b.StopTimer()
			cancel()
			wg.Wait()
			select {
			case err := <-errs:
				b.Fatal(err)
			default:
			}
			if samples.count != b.N {
				b.Fatalf("measured %d transactions, want %d", samples.count, b.N)
			}
			tail := slices.Clone(samples.tail[:min(samples.count, len(samples.tail))])
			slices.Sort(tail)
			b.ReportMetric(float64(samples.max.Nanoseconds()), "max-held-ns")
			b.ReportMetric(float64(tail[(len(tail)-1)*95/100].Nanoseconds()), "p95-held-ns")
			b.ReportMetric(float64(reads.Load()), "reader-calls")
			b.ReportMetric(float64(s.db.Stats().WaitDuration.Nanoseconds()), "writer-wait-ns")
		})
	}
}
