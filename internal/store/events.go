package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"unicode/utf8"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/stateport"
)

func (s *Store) insertEvent(ctx context.Context, tx *sql.Tx, input stateport.EventWrite) (int64, error) {
	event := input.Event
	event.ID = 0
	var id int64
	var previous string
	err := s.progressRow(ctx, tx, `SELECT id,value FROM events WHERE dedupe=?`, input.Dedupe).Scan(&id, &previous)
	if err == nil {
		if previous != encode(event) {
			return 0, refuse(eventDedupe, nil)
		}
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, databaseError(err)
	}
	result, err := s.progressExec(ctx, tx, `INSERT INTO events(repo,kind,dedupe,at,value) VALUES(?,?,?,?,?)`, stateport.EventScope(event), event.Kind,
		input.Dedupe, event.At.UTC().Format(timestampFormat), encode(event))
	if err != nil {
		return 0, databaseError(err)
	}
	id, err = result.LastInsertId()
	if err == nil {
		s.noteChange(stateport.Change{Repository: event.Repo, Run: event.RunID, Upstream: stateport.EventScope(event)})
	}
	return id, databaseError(err)
}

func (s *Store) Events(ctx context.Context, filter stateport.EventFilter) (model.EventPage, error) {
	started := s.options.Clock()
	defer func() { s.metrics.events.record(s.options.Clock().Sub(started)) }()
	repositories, after := filter.ScopeKeys(), filter.After
	result := model.EventPage{Events: make([]model.AgentEvent, 0), NextAfterID: after}
	if !filter.Valid() {
		return result, refuse(invalid, nil)
	}
	tx, err := s.readers.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return result, databaseError(err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := eventBounds(ctx, tx, repositories, &result); err != nil {
		return result, err
	}
	through := filter.Through
	if through == 0 {
		through = result.LatestID
	}
	query, queryArgs := eventQuery(filter, through)
	rows, err := tx.QueryContext(ctx, query, queryArgs...)
	if err != nil {
		return result, databaseError(err)
	}
	defer func() { _ = rows.Close() }()
	if err := readEventRows(rows, &result, filter); err != nil {
		return result, err
	}
	result.HasMore = result.NextAfterID < min(result.LatestID, through)
	if err := rows.Close(); err != nil {
		return result, databaseError(err)
	}
	result.Gap, err = eventGap(ctx, tx, filter)
	return result, databaseError(err)
}

func readEventRows(rows *sql.Rows, result *model.EventPage, filter stateport.EventFilter) error {
	limit := filter.Limit
	scanned := 0
	for rows.Next() {
		scanned++
		var event model.AgentEvent
		var body string
		if err := rows.Scan(&event.ID, &body); err != nil {
			return databaseError(err)
		}
		id := event.ID
		if err := json.Unmarshal([]byte(body), &event); err != nil {
			return refuse(corrupt, err)
		}
		event.ID = id
		result.NextAfterID = id
		if filter.Selects(event) {
			result.Events = append(result.Events, event)
		}
		if len(result.Events) == limit {
			break
		}
	}
	if len(result.Events) < limit && scanned < eventScanLimit(filter) {
		through := filter.Through
		if through == 0 {
			through = result.LatestID
		}
		result.NextAfterID = max(result.NextAfterID, min(result.LatestID, through))
	}
	return databaseError(rows.Err())
}

func validateEvent(input stateport.EventWrite) error {
	event := input.Event
	if !model.EventKind(event.Kind).Valid() ||
		!eventRepository(event) ||
		input.Dedupe == "" ||
		len(input.Dedupe) > 512 ||
		len(event.Data) > model.MaxEventDataBytes ||
		(len(event.Data) > 0 &&
			!json.Valid(event.Data)) ||
		utf8.RuneCountInString(event.Summary) > model.MaxEventSummaryRunes {
		return refuse(invalid, nil)
	}
	return nil
}

func eventBounds(ctx context.Context, tx *sql.Tx, repositories []string, result *model.EventPage) error {
	predicate, args := scopePredicate("repo", repositories)
	for _, endpoint := range []struct {
		order string
		value *int64
	}{{"ASC", &result.OldestID}, {"DESC", &result.LatestID}} {
		err := tx.QueryRowContext(ctx, `SELECT id FROM events WHERE `+predicate+` ORDER BY id `+endpoint.order+` LIMIT 1`, args...).Scan(endpoint.value)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return databaseError(err)
		}
	}
	var pruned int64
	err := tx.QueryRowContext(ctx, `SELECT coalesce(max(through),0) FROM event_pruned WHERE `+predicate, args...).Scan(&pruned)
	if err != nil {
		return databaseError(err)
	}
	result.LatestID = max(result.LatestID, pruned)
	return nil
}

func eventQuery(f stateport.EventFilter, through int64) (string, []any) {
	predicate, args := scopePredicate("e.repo", f.ScopeKeys())
	from := `events e`
	if f.Actor != "" {
		// Unary plus removes TEXT affinity so the expression index can seek the run ID.
		from = `runs r INDEXED BY run_actor CROSS JOIN events e INDEXED BY event_run
ON json_extract(e.value,'$.run_id')=+r.id JOIN repositories p ON p.id=r.repo_id AND p.canonical=e.repo`
		if f.After > 0 {
			// A wake reads only the new event window, then checks its run's owner.
			from = `events e CROSS JOIN runs r ON r.id=json_extract(e.value,'$.run_id')
JOIN repositories p ON p.id=r.repo_id AND p.canonical=e.repo`
		}
		predicate += ` AND json_extract(r.origin,'$.actor')=?`
		args = append(args, f.Actor)
	}
	if len(f.RunIDs) > 0 {
		selection, values := scopePredicate("json_extract(e.value,'$.run_id')", f.RunIDs)
		predicate += " AND " + selection
		args = append(args, values...)
	}
	if len(f.Kinds) > 0 {
		kinds := make([]string, len(f.Kinds))
		for i, kind := range f.Kinds {
			kinds[i] = string(kind)
		}
		selection, values := scopePredicate("e.kind", kinds)
		predicate += " AND " + selection
		args = append(args, values...)
	}
	query := `SELECT e.id,e.value FROM ` + from + ` WHERE ` + predicate + ` AND e.id>? AND e.id<=? ORDER BY e.id LIMIT ?`
	return query, append(args, f.After, through, eventScanLimit(f))
}

func eventScanLimit(f stateport.EventFilter) int {
	if len(f.RefsGlob) > 0 {
		return stateport.EventPageLimit
	}
	return f.Limit
}

func eventRepository(v model.AgentEvent) bool {
	if v.Repo == "" {
		return model.EventKind(v.Kind).Global()
	}
	return validRepository(v.Repo)
}

// RecordEvents appends best-effort native notices without a run-control write.
func (s *Store) RecordEvents(ctx context.Context, events []stateport.EventWrite) error {
	if len(events) == 0 {
		return nil
	}
	return s.observe(ctx, func(*sql.Tx) error { return nil }, events...)
}
