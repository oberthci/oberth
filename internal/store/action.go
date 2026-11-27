package store

import (
	"context"
	"database/sql"
	"encoding/json"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/stateport"
)

func validateEvents(events []stateport.EventWrite) error {
	for _, event := range events {
		if err := validateEvent(event); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) writeEvents(ctx context.Context, tx *sql.Tx, events []stateport.EventWrite) error {
	for _, event := range events {
		if _, err := s.insertEvent(ctx, tx, event); err != nil {
			return err
		}
	}
	return nil
}

// RecordAction appends a caller-owned administrative fact without exposing SQL.
func (s *Store) RecordAction(ctx context.Context, action model.AuditAction, events ...stateport.EventWrite) error {
	m, err := actionMutation(action)
	if err != nil {
		return err
	}
	return s.change(ctx, m, func(*sql.Tx) error { return nil }, events...)
}

func actionMutation(action model.AuditAction) (mutation, error) {
	if action.Actor == "" {
		return mutation{}, refuse(invalid, nil)
	}
	if action.Action == "" || action.ResourceType == "" {
		return mutation{}, refuse(invalid, nil)
	}
	var details any
	if action.Details != "" {
		details = json.RawMessage(action.Details)
	}
	m := mutation{action.Actor, action.Action, action.ResourceType, action.ResourceID, details}
	return m, nil
}
