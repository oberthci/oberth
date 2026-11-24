package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/oberthci/oberth/internal/stateport"
)

// This connection-local index is disposable. Schema 6's receive receipts are
// the only durable source; neither snapshots nor upgrades need another table.
func ensureReceivedIndex(ctx context.Context, tx *sql.Tx) error {
	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_temp_master WHERE name='receive_heads'`).Scan(&exists); err != nil {
		return databaseError(err)
	}
	var version int64
	if err := tx.QueryRowContext(ctx, `PRAGMA data_version`).Scan(&version); err != nil {
		return databaseError(err)
	}
	if exists != 0 {
		var indexed int64
		if err := tx.QueryRowContext(ctx, `SELECT data_version FROM temp.receive_index_version`).Scan(&indexed); err != nil {
			return databaseError(err)
		}
		if indexed == version {
			return nil
		}
	} else if _, err := tx.ExecContext(ctx, `CREATE TEMP TABLE receive_heads(
repo_id INTEGER NOT NULL, ref TEXT NOT NULL, object_sha TEXT NOT NULL,
heads BLOB NOT NULL, PRIMARY KEY(repo_id,ref));
CREATE TEMP TABLE receive_index_version(singleton INTEGER PRIMARY KEY CHECK(singleton=1), data_version INTEGER NOT NULL)`); err != nil {
		return databaseError(err)
	}
	return rebuildReceivedIndex(ctx, tx, version)
}

func rebuildReceivedIndex(ctx context.Context, tx *sql.Tx, version int64) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM temp.receive_heads`); err != nil {
		return databaseError(err)
	}
	rows, err := tx.QueryContext(ctx, `SELECT repo_id,binding FROM receives ORDER BY rowid`)
	if err != nil {
		return databaseError(err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var repoID int64
		var body []byte
		if err := rows.Scan(&repoID, &body); err != nil {
			return databaseError(err)
		}
		if err := indexReceiveReceipt(ctx, tx, repoID, body); err != nil {
			return err
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return databaseError(err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO temp.receive_index_version VALUES(1,?) ON CONFLICT(singleton) DO UPDATE SET data_version=excluded.data_version`, version); err != nil {
		return databaseError(err)
	}
	return nil
}

func indexReceiveReceipt(ctx context.Context, tx *sql.Tx, repoID int64, body []byte) error {
	var binding receiveBinding
	if err := json.Unmarshal(body, &binding); err != nil {
		return refuse(corrupt, err)
	}
	for _, r := range binding.Received {
		entry := receivedIndexEntry{repoID: repoID, ref: r.Ref, object: r.ObjectSHA, heads: receiptHeads(binding, r)}
		if err := indexReceivedHeads(ctx, tx, entry); err != nil {
			return err
		}
	}
	return nil
}

type receivedIndexEntry struct {
	repoID      int64
	ref, object string
	heads       []string
}

func indexReceivedHeads(ctx context.Context, tx *sql.Tx, entry receivedIndexEntry) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO temp.receive_heads VALUES(?,?,?,?)
ON CONFLICT(repo_id,ref) DO UPDATE SET object_sha=excluded.object_sha,heads=excluded.heads`, entry.repoID, entry.ref, entry.object, encode(entry.heads))
	return databaseError(err)
}

func captureReceivedHeads(ctx context.Context, tx *sql.Tx, repoID int64, batch preparedBatch) ([]byte, error) {
	if len(batch.Received) == 0 {
		return batch.body, nil
	}
	if err := ensureReceivedIndex(ctx, tx); err != nil {
		return nil, err
	}
	binding := batch.binding
	binding.Heads = make(map[string][]string, len(batch.Received))
	for _, r := range batch.Received {
		var object string
		var body []byte
		err := tx.QueryRowContext(ctx, `SELECT object_sha,heads FROM temp.receive_heads WHERE repo_id=? AND ref=?`, repoID, r.Ref).Scan(&object, &body)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, databaseError(err)
		}
		var previous []string
		if body != nil {
			if err := json.Unmarshal(body, &previous); err != nil {
				return nil, refuse(corrupt, err)
			}
		}
		heads := stateport.InheritReceivedHeads(r.PreviousSHA, object, previous)
		binding.Heads[r.Ref] = heads
		if err := indexReceivedHeads(ctx, tx, receivedIndexEntry{repoID: repoID, ref: r.Ref, object: r.ObjectSHA, heads: heads}); err != nil {
			return nil, err
		}
	}
	return json.Marshal(binding)
}

func (s *Store) lastReceivedHeads(ctx context.Context, q stateport.ReceivedRef) ([]string, error) {
	tx, done, err := s.beginWrite(ctx)
	if err != nil {
		return nil, err
	}
	defer done()
	if err := ensureReceivedIndex(ctx, tx); err != nil {
		return nil, err
	}
	var body []byte
	err = tx.QueryRowContext(ctx, `SELECT h.heads FROM temp.receive_heads h
JOIN repositories r ON r.id=h.repo_id WHERE r.canonical=? AND h.ref=? AND h.object_sha=?`, q.Repo, q.Ref, q.ObjectSHA).Scan(&body)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, databaseError(err)
	}
	var heads []string
	if body != nil {
		if err := json.Unmarshal(body, &heads); err != nil {
			return nil, refuse(corrupt, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, databaseError(err)
	}
	return heads, nil
}
