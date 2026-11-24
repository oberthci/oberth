package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/stateport"
)

// ReceivedHeads reads immutable receive evidence, never Git tracking state.
func (s *Store) ReceivedHeads(ctx context.Context, q stateport.ReceivedRef) ([]string, error) {
	if q.RunID == "" {
		return s.lastReceivedHeads(ctx, q)
	}
	seen := map[string]bool{}
	for q.RunID != "" && !seen[q.RunID] {
		seen[q.RunID] = true
		var body []byte
		err := s.readers.QueryRowContext(ctx, `SELECT r.binding FROM receives r
JOIN receive_runs rr ON rr.receive_id=r.id WHERE rr.run_id=?`, q.RunID).Scan(&body)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		if err != nil {
			return nil, databaseError(err)
		}
		var binding receiveBinding
		if err := json.Unmarshal(body, &binding); err != nil {
			return nil, refuse(corrupt, err)
		}
		heads, previous := bindingReceivedHeads(q, binding)
		if heads != nil {
			return heads, nil
		}
		q.RunID = previous
	}
	return nil, nil
}

func bindingReceivedHeads(q stateport.ReceivedRef, binding receiveBinding) ([]string, string) {
	for _, received := range binding.Received {
		if received.Repo == q.Repo && received.Ref == q.Ref && received.ObjectSHA == q.ObjectSHA {
			return receiptHeads(binding, received), ""
		}
	}
	for _, run := range binding.Runs {
		if run.RunID == q.RunID {
			return nil, run.RerunOf
		}
	}
	return nil, ""
}

func receiptHeads(binding receiveBinding, r model.CIRequest) []string {
	if heads := binding.Heads[r.Ref]; len(heads) > 0 {
		return slices.Clone(heads)
	}
	return []string{r.PreviousSHA}
}

// Derived evidence is not part of the caller's replay identity.
func receivedRequest(body []byte) ([]byte, error) {
	var binding receiveBinding
	if err := json.Unmarshal(body, &binding); err != nil {
		return nil, refuse(corrupt, err)
	}
	binding.Heads = nil
	return json.Marshal(binding)
}

func prepareReceived(input stateport.BatchAcceptance) ([]model.CIRequest, error) {
	if len(input.Received) > stateport.MaxReceivedRefs {
		return nil, refuse(invalid, nil)
	}
	refs := map[string]bool{}
	received := make([]model.CIRequest, len(input.Received))
	for i, r := range input.Received {
		if refs[r.Ref] {
			return nil, refuse(invalid, nil)
		}
		refs[r.Ref] = true
		if err := validateReceived(input, r); err != nil {
			return nil, err
		}
		received[i] = r.Clone()
	}
	return received, nil
}

func validateReceived(input stateport.BatchAcceptance, r model.CIRequest) error {
	if r.ReceiveID != input.ReceiveID || r.Repo != input.Repository || encode(r.Actor) != encode(input.Actor) || !stateport.ValidReceivedRef(r) {
		return refuse(invalid, nil)
	}
	return nil
}
