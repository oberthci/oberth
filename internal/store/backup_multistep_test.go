package store

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"modernc.org/sqlite"
)

func TestOnlineBackupMultiStepPinnedViewWithWriter(t *testing.T) {
	path, source := backupFixture(t)
	// About 6,000 pages, far beyond a Step(128). The writer mutates early,
	// middle and late pages plus a cross-table transactional invariant.
	if _, err := source.db.Exec(`
CREATE TABLE backup_a(n INTEGER NOT NULL);
INSERT INTO backup_a VALUES(0);
CREATE TABLE backup_b(n INTEGER NOT NULL);
CREATE TABLE backup_pad(id INTEGER PRIMARY KEY, data BLOB);
WITH RECURSIVE n(i) AS (VALUES(1) UNION ALL SELECT i+1 FROM n WHERE i<6000)
INSERT INTO backup_pad SELECT i, zeroblob(3000) FROM n;
PRAGMA wal_checkpoint(PASSIVE);`); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second) // Liveness only; channels synchronize every commit.
	defer cancel()
	requests := make(chan struct{})
	committed := make(chan error)
	joined := make(chan struct{})
	go func() {
		defer close(joined)
		for range requests {
			err := func() error {
				tx, err := source.db.BeginTx(ctx, nil)
				if err != nil {
					return err
				}
				defer func() { _ = tx.Rollback() }()
				_, err = tx.ExecContext(ctx, `UPDATE backup_a SET n=n+1;
INSERT INTO backup_b SELECT n FROM backup_a;
UPDATE backup_pad SET data=randomblob(3000) WHERE id IN (1,3000,6000)`)
				if err != nil {
					return err
				}
				if err := tx.Commit(); err != nil {
					return err
				}
				_, err = source.db.ExecContext(ctx, "PRAGMA wal_checkpoint(PASSIVE)")
				return err
			}()
			select {
			case committed <- err:
			case <-ctx.Done():
				return
			}
		}
	}()
	t.Cleanup(func() { close(requests); cancel(); <-joined })
	for round := 0; round < 5; round++ {
		var pinned int
		if err := source.db.QueryRowContext(ctx, "SELECT n FROM backup_a").Scan(&pinned); err != nil {
			t.Fatal(err)
		}
		steps, writes := 0, 0
		snapshot, cleanup, err := snapshotOnlineDatabaseSteps(ctx, path, DefaultBackupMaxBytes, func(backup *sqlite.Backup) (bool, error) {
			steps++
			more, err := backup.Step(128)
			if err != nil || !more {
				return more, err
			}
			// Force a real concurrent commit AFTER every non-final step, while
			// the pinned read transaction and SQLite backup handle are alive.
			select {
			case requests <- struct{}{}:
			case <-ctx.Done():
				return false, ctx.Err()
			}
			select {
			case err = <-committed:
				writes++
				return more, err
			case <-ctx.Done():
				return false, ctx.Err()
			}
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = cleanup() })
		if steps < 20 || writes != steps-1 {
			t.Fatalf("fixture did not exercise multiple committed steps: steps=%d writes=%d", steps, writes)
		}
		copyDB, err := sql.Open("sqlite", currentDatabaseDSN(snapshot, "ro", false))
		if err != nil {
			t.Fatal(err)
		}
		var a, b int
		var integrity string
		if err := copyDB.QueryRowContext(ctx, "SELECT (SELECT n FROM backup_a), (SELECT count(*) FROM backup_b)").Scan(&a, &b); err != nil {
			t.Fatal(err)
		}
		if err := copyDB.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&integrity); err != nil {
			t.Fatal(err)
		}
		if err := copyDB.Close(); err != nil {
			t.Fatal(err)
		}
		if a != b || a != pinned || integrity != "ok" {
			t.Fatalf("round %d mixed committed views: pinned=%d a=%d b=%d integrity=%s", round, pinned, a, b, integrity)
		}
		var live int
		if err := source.db.QueryRowContext(ctx, "SELECT n FROM backup_a").Scan(&live); err != nil || live != pinned+writes {
			t.Fatal("writer did not advance independently of snapshot", live, pinned, writes, err)
		}
		t.Logf("round %d: %d steps, %d interleaved commits, pinned=%d live=%d", round, steps, writes, pinned, live)
	}
}
