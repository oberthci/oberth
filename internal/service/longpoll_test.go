package service

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
)

type countedWaitStore struct {
	timeoutRunStore
	reads int
}

func (s *countedWaitStore) ResolveWaitRun(ctx context.Context, repo int64, sha, trigger string) (model.Run, error) {
	s.reads++
	return s.timeoutRunStore.ResolveWaitRun(ctx, repo, sha, trigger)
}

func TestPreparedLongPollValidatesBeforeObservation(t *testing.T) {
	t.Parallel()
	runs := &countedWaitStore{timeoutRunStore: timeoutRunStore{repo: model.Repository{ID: 1, Name: "oberth"}, run: model.Run{ID: "exact-run", RepoID: 1, SHA: strings.Repeat("a", 40), Status: model.RunPassed}}}
	control, err := NewAPI(APIConfig{Runs: runs, Signals: NewSignals()})
	if err != nil {
		t.Fatal(err)
	}
	actor := api.Actor{Identity: "agent@host"}
	for _, raw := range []string{`{"sha":"main"}`, `{"sha":"aaaaaaaa","unknown":true}`, `{"sha":"aaaaaaaa","timeout":601}`, `{"sha":"aaaaaaaa","timeout":-1}`} {
		if prepared, err := control.PrepareLongPoll(context.Background(), actor, "wait", json.RawMessage(raw)); err == nil || prepared != nil {
			t.Fatalf("invalid arguments accepted: %s", raw)
		}
	}
	if runs.reads != 0 {
		t.Fatal("invalid request queried target")
	}
	raw := json.RawMessage(`{"sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`)
	if _, err := control.PrepareLongPoll(context.Background(), api.Actor{}, "wait", raw); err == nil {
		t.Fatal("missing identity accepted")
	}
	if _, err := control.PrepareLongPoll(context.Background(), actor, "cancel", raw); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("mutating tool accepted: %v", err)
	}
	prepared, err := control.PrepareLongPoll(context.Background(), actor, "wait", raw)
	if err != nil {
		t.Fatal(err)
	}
	if runs.reads != 1 {
		t.Fatalf("preflight reads=%d, want1", runs.reads)
	}
	value, err := prepared(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if runs.reads != 1 {
		t.Fatal("prepared terminal snapshot was queried twice")
	}
	response, ok := value.(WaitResponse)
	if !ok || response.Run.ID != "exact-run" || response.StillRunning {
		t.Fatalf("response %#v", value)
	}
}

func TestPreparedLongPollCancellationPreservesDurableWork(t *testing.T) {
	fixture := newControlFixture(t)
	control := fixture.api(t)
	ctx := context.Background()
	run, err := fixture.scheduler.EnqueueCI(ctx, CIRequest{EventID: "stream-cancel", Repository: fixture.repo, Branch: "feature/stream", SHA: strings.Repeat("a", 40), Actor: "agent@host"})
	if err != nil {
		t.Fatal(err)
	}
	promotion, err := fixture.store.AppendPromotion(ctx, model.PromotionSpec{RepoID: fixture.repo.ID, SourceBranch: "feature/stream", SourceSHA: run.SHA, TargetRef: "main", Actor: "agent@host"})
	if err != nil {
		t.Fatal(err)
	}
	before, err := fixture.store.VerifyAuditChain(ctx)
	if err != nil {
		t.Fatal(err)
	}
	control.mutationGate = func(context.Context) error { t.Error("observer invoked mutation gate"); return errors.New("forbidden") }
	for _, tc := range []struct {
		name string
		args map[string]any
	}{{"wait", map[string]any{"repo": fixture.repo.Name, "sha": run.SHA}}, {"promote_status", map[string]any{"id": promotion.ID}}} {
		raw, err := json.Marshal(tc.args)
		if err != nil {
			t.Fatal(err)
		}
		prepared, err := control.PrepareLongPoll(ctx, api.Actor{Identity: "agent@host"}, tc.name, raw)
		if err != nil {
			t.Fatal(err)
		}
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		if _, err := prepared(cancelled); !errors.Is(err, context.Canceled) {
			t.Fatalf("%s cancellation: %v", tc.name, err)
		}
	}
	after, err := fixture.store.VerifyAuditChain(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatal("observation cancellation mutated audit")
	}
	gotRun, err := fixture.store.Run(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	gotPromotion, err := fixture.store.Promotion(ctx, promotion.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotRun.Status != run.Status || gotPromotion.Status != promotion.Status {
		t.Fatalf("durable work changed: run%s promotion%s", gotRun.Status, gotPromotion.Status)
	}
	for _, raw := range []string{`{"id":"absent"}`, `{"id":""}`, `{"id":"absent","unknown":1}`} {
		if prepared, err := control.PrepareLongPoll(ctx, api.Actor{Identity: "agent@host"}, "promote_status", json.RawMessage(raw)); err == nil || prepared != nil {
			t.Fatalf("invalid/missing promotion prepared: %s", raw)
		}
	}
}
