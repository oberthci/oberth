package store

import (
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/stateport"
)

type atomicFact struct {
	name    string
	prepare func(*testing.T, *Store) func(stateport.Action) error
}

func checkAtomicFacts(t *testing.T, facts []atomicFact) {
	t.Helper()
	for _, fact := range facts {
		t.Run(fact.name, func(t *testing.T) {
			s := testStore(t)
			write := fact.prepare(t, s)
			before := databaseContents(t, s.readers)
			// The first event has already been inserted when the second event fails.
			execute(t, s.writer, `CREATE TRIGGER reject_event BEFORE INSERT ON events WHEN NEW.dedupe='atomic-second' BEGIN SELECT RAISE(ABORT,'injected event failure'); END`)
			action := stateport.Action{Actor: "operator", Events: []stateport.EventWrite{atomicEvent(s, "atomic-first"), atomicEvent(s, "atomic-second")}}
			err := write(action)
			if err == nil || !strings.Contains(fmt.Sprint(err), "injected") && !hasInjectedCause(err) {
				t.Fatalf("did not reach the injected failure: %v", err)
			}
			if got := databaseContents(t, s.readers); got != before {
				t.Fatal("failed transaction retained fact, audit, event, projection or reservation")
			}
			execute(t, s.writer, `DROP TRIGGER reject_event`)
			requireOK(t, write(action))
			page, err := s.Events(t.Context(), stateport.EventFilter{Limit: 500})
			requireOK(t, err)
			if len(page.Events) != 2 {
				t.Fatal("fact did not commit its two events", page)
			}
		})
	}
}

func hasInjectedCause(err error) bool {
	for err != nil {
		if strings.Contains(err.Error(), "injected event failure") {
			return true
		}
		next, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = next.Unwrap()
	}
	return false
}

func atomicEvent(s *Store, key string) stateport.EventWrite {
	return stateport.EventWrite{Dedupe: key, Event: model.AgentEvent{Repo: "origin/team/repo", Kind: "run.updated", At: s.options.Clock(), Summary: key}}
}

func databaseContents(t *testing.T, db *sql.DB) string {
	t.Helper()
	rows, err := db.QueryContext(t.Context(), `SELECT name FROM sqlite_master WHERE type='table' ORDER BY name`)
	requireOK(t, err)
	var tables []string
	for rows.Next() {
		var table string
		requireOK(t, rows.Scan(&table))
		tables = append(tables, table)
	}
	requireOK(t, rows.Err())
	requireOK(t, rows.Close())
	var result strings.Builder
	for _, table := range tables {
		result.WriteString(table)
		result.WriteString(tableContents(t, db, table))
	}
	return result.String()
}

func tableContents(t *testing.T, db *sql.DB, table string) string {
	t.Helper()
	rows, err := db.QueryContext(t.Context(), `SELECT * FROM "`+table+`"`)
	requireOK(t, err)
	defer func() { requireOK(t, rows.Close()) }()
	columns, err := rows.Columns()
	requireOK(t, err)
	var records []string
	for rows.Next() {
		values := make([]any, len(columns))
		pointers := make([]any, len(columns))
		for i := range values {
			pointers[i] = &values[i]
		}
		requireOK(t, rows.Scan(pointers...))
		records = append(records, encode(values))
	}
	requireOK(t, rows.Err())
	slices.Sort(records)
	return strings.Join(records, "\n")
}
