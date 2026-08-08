package service

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/oberthci/oberth/internal/model"
)

type orderedAuditor struct {
	events     *[]string
	rejectNext bool
}

func (auditor *orderedAuditor) AppendAuditAction(_ context.Context, spec model.AuditActionSpec) (model.AuditAction, error) {
	*auditor.events = append(*auditor.events, "audit:"+spec.Action)
	if auditor.rejectNext {
		auditor.rejectNext = false
		return model.AuditAction{}, errors.New("audit unavailable")
	}
	return model.AuditAction{Action: spec.Action}, nil
}

func TestAuditedGitMutationOrdersIntentMutationAndOutcome(t *testing.T) {
	for name, mutationErr := range map[string]error{
		"success": nil,
		"failure": errors.New("remote rejected mutation"),
	} {
		t.Run(name, func(t *testing.T) {
			var events []string
			auditor := &orderedAuditor{events: &events}
			err := auditedGitMutation(context.Background(), auditor, "agent@host", "sync.branch", "run", "run-1",
				map[string]any{"sha": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}, func() error {
					events = append(events, "git")
					return mutationErr
				})
			if !errors.Is(err, mutationErr) {
				t.Fatalf("mutation error = %v, want %v", err, mutationErr)
			}
			outcome := "audit:sync.branch.succeeded"
			if mutationErr != nil {
				outcome = "audit:sync.branch.failed"
			}
			want := []string{"audit:sync.branch.intent", "git", outcome}
			if !slices.Equal(events, want) {
				t.Fatalf("ordered events = %#v, want %#v", events, want)
			}
		})
	}
}

func TestAuditedGitMutationFailsClosedBeforeGit(t *testing.T) {
	var events []string
	auditor := &orderedAuditor{events: &events, rejectNext: true}
	err := auditedGitMutation(context.Background(), auditor, "agent@host", "promotion.push", "promotion", "promotion-1", nil, func() error {
		events = append(events, "git")
		return nil
	})
	if err == nil {
		t.Fatal("mutation unexpectedly succeeded without durable intent")
	}
	want := []string{"audit:promotion.push.intent"}
	if !slices.Equal(events, want) {
		t.Fatalf("events after rejected intent = %#v, want %#v", events, want)
	}
}
