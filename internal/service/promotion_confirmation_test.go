package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
)

type confirmationGit struct {
	*controlGit
	after, readError string
	pushed, reads    int
	pushError        error
}

func (g *confirmationGit) PushPromotion(_ context.Context, _, _, _ string) error {
	g.pushed++
	return g.pushError
}

func (g *confirmationGit) RemoteRef(ctx context.Context, repo, ref string) (string, bool, error) {
	g.reads++
	if g.pushed == 0 {
		return g.controlGit.RemoteRef(ctx, repo, ref)
	}
	if g.readError != "" {
		return "", false, errors.New(g.readError)
	}
	return g.after, g.after != "", nil
}

func TestPromotionConfirmsForgeBeforePassing(t *testing.T) {
	for _, tc := range []struct {
		name, after, readError string
		pushError              error
		want                   model.PromotionStatus
	}{
		{name: "confirmed", after: retrySourceSHA, want: model.PromotionPassed},
		{name: "lost push reply", after: retrySourceSHA, pushError: errors.New("connection reset by peer"), want: model.PromotionPassed},
		{name: "acknowledged without update", after: retryBaseSHA, want: model.PromotionFailed},
		{name: "missing", want: model.PromotionFailed},
		{name: "moved", after: retryMovedSHA, want: model.PromotionFailed},
		{name: "unreadable", readError: "permission denied (publickey)", want: model.PromotionFailed},
		{name: "transient read failure", readError: "connection reset by peer", want: model.PromotionPending},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAgentFixture(t)
			p, err := f.store.AppendPromotion(t.Context(), model.PromotionSpec{RepoID: f.repo.ID, SourceBranch: "feature/confirm", SourceSHA: retrySourceSHA, TargetRef: "main", PreviousSHA: retryBaseSHA, ResultSHA: retrySourceSHA, Actor: "agent@host"})
			if err != nil {
				t.Fatal(err)
			}
			pub, err := f.store.BeginPublication(t.Context(), model.PublicationSpec{RepoID: f.repo.ID, PromotionID: p.ID, RefKind: model.RefBranch, Ref: "main", PreviousSHA: p.PreviousSHA, ResultSHA: p.ResultSHA, Actor: p.Actor})
			if err != nil {
				t.Fatal(err)
			}
			f.git.setRemoteRefLocked("refs/heads/main", retryBaseSHA)
			g := &confirmationGit{controlGit: f.git, after: tc.after, readError: tc.readError, pushError: tc.pushError}
			final, err := deliverPublication(t.Context(), f.store, g, f.store, f.repo, "forge/team/project", pub)
			if err != nil {
				t.Fatal(err)
			}
			loaded, err := f.store.Promotion(t.Context(), p.ID)
			if err != nil || loaded.Status != tc.want || g.reads != 2 || g.pushed != 1 {
				t.Fatalf("promotion=%+v reads=%d pushes=%d err=%v", loaded, g.reads, g.pushed, err)
			}
			wantSHA := ""
			if tc.want == model.PromotionPassed {
				wantSHA = tc.after
			}
			if loaded.ObservedTargetSHA != wantSHA {
				t.Fatalf("observation=%q want %q", loaded.ObservedTargetSHA, wantSHA)
			}
			if tc.want == model.PromotionPending {
				// Restart/retry can confirm a delivered result without pushing again.
				g.readError = ""
				g.after = retrySourceSHA
				final.Publication, err = f.store.UpdatePublicationAttempt(t.Context(), pub.ID, final.Publication.Attempts, final.Publication.Attempts, time.Now().Add(-time.Second), final.Publication.Error)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = deliverPublication(t.Context(), f.store, g, f.store, f.repo, "forge/team/project", final.Publication); err != nil {
					t.Fatal(err)
				}
				loaded, err = f.store.Promotion(t.Context(), p.ID)
				if err != nil || loaded.Status != model.PromotionPassed || loaded.ObservedTargetSHA != retrySourceSHA || g.pushed != 1 {
					t.Fatalf("recovery=%+v err=%v", loaded, err)
				}
				wantSHA = retrySourceSHA
			}
			result := resultCall(t, resultServer(t, f.api(t), api.Actor{Identity: "admin@host", Admin: true}), "promote_status", map[string]any{"id": p.ID, "timeout_s": 0})
			if wantSHA != "" && !strings.Contains(string(result.Structured), `"observed_target_sha":"`+wantSHA+`"`) {
				t.Fatalf("status lost confirmation: %s", result.Structured)
			}
			if wantSHA == "" && strings.Contains(string(result.Structured), `"observed_target_sha"`) {
				t.Fatalf("unconfirmed status claimed observation: %s", result.Structured)
			}
			f.recorder.observePromotion(t.Context(), loaded)
			page := f.events(t, agentActor, map[string]any{"kinds": []string{model.AgentEventPromotionFinished}})
			if len(page.Events) != 1 {
				t.Fatalf("promotion events = %+v", page.Events)
			}
			observation, _ := page.Events[0].Data["observed_target_sha"].(string)
			if observation != wantSHA {
				t.Fatalf("event observation=%q want %q", observation, wantSHA)
			}
		})
	}
}

func TestReceiveOutboxArgumentErrorsOverMCP(t *testing.T) {
	f := newControlFixture(t)
	control := f.api(t)
	control.SetReceiveOutbox(&fakeOutbox{}, nil)
	server := resultServer(t, control, api.Actor{Identity: "admin@host", Admin: true})
	for _, tc := range []struct {
		args map[string]any
		want string
	}{
		{map[string]any{"action": "list", "limit": 5}, `unknown field "limit"; accepted arguments: action, id, reason, repo`},
		{map[string]any{"action": 5}, "action must be string"},
		{map[string]any{"action": "show", "repo": true}, "repo must be string"},
		{map[string]any{}, "action must be list, show, retry or abandon"},
		{map[string]any{"action": "inspect"}, "action must be list, show, retry or abandon"},
		{map[string]any{"action": "show"}, "repo is required for show"},
		{map[string]any{"action": "abandon", "repo": "forge/team/project"}, "reason for abandon must be 1..512 bytes"},
	} {
		result := resultCall(t, server, "receive_outbox", tc.args)
		assertRefusal(t, result, "invalid input: "+tc.want, "")
	}
}
