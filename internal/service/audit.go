package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/oberthci/oberth/internal/model"
)

// auditedGitMutation persists actor-bound intent before an upstream mutation,
// then records its terminal outcome. A failed intent write prevents Git from
// being touched; a failed outcome write is joined with the mutation result.
func auditedGitMutation(
	ctx context.Context,
	auditor Auditor,
	actor, action, resourceType, resourceID string,
	details map[string]any,
	mutate func() error,
) error {
	if auditor == nil {
		return fmt.Errorf("%w: audit", ErrUnavailable)
	}
	intentDetails, err := json.Marshal(details)
	if err != nil {
		return fmt.Errorf("encode Git mutation intent: %w", err)
	}
	if _, err := auditor.AppendAuditAction(ctx, model.AuditActionSpec{
		Actor: actor, Action: action + ".intent", ResourceType: resourceType,
		ResourceID: resourceID, Details: string(intentDetails),
	}); err != nil {
		return fmt.Errorf("persist Git mutation intent: %w", err)
	}
	mutationErr := mutate()
	outcome := make(map[string]any, len(details)+2)
	for key, value := range details {
		outcome[key] = value
	}
	if mutationErr == nil {
		outcome["outcome"] = "succeeded"
	} else {
		outcome["outcome"] = "failed"
		outcome["error"] = mutationErr.Error()
	}
	outcomeDetails, encodeErr := json.Marshal(outcome)
	if encodeErr != nil {
		return errors.Join(mutationErr, fmt.Errorf("encode Git mutation outcome: %w", encodeErr))
	}
	outcomeAction := action + ".succeeded"
	if mutationErr != nil {
		outcomeAction = action + ".failed"
	}
	_, auditErr := auditor.AppendAuditAction(ctx, model.AuditActionSpec{
		Actor: actor, Action: outcomeAction, ResourceType: resourceType,
		ResourceID: resourceID, Details: string(outcomeDetails),
	})
	if auditErr != nil {
		auditErr = fmt.Errorf("persist Git mutation outcome: %w", auditErr)
	}
	return errors.Join(mutationErr, auditErr)
}
