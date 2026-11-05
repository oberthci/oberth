package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
)

func TestConcurrencyGroupsMCPEnvelope(t *testing.T) {
	for _, mode := range []string{"empty", "idle", "holders-and-waiter", "nil-observer-list"} {
		t.Run(mode, func(t *testing.T) {
			f := newControlFixture(t)
			control := f.api(t)
			f.scheduler.admission = newWorkflowAdmissionForJobs(3)
			control.admission = f.scheduler
			if mode == "nil-observer-list" {
				control.admission = &concurrencyResultObserver{}
			}
			if mode == "idle" || mode == "holders-and-waiter" {
				for index := range 3 {
					run, err := f.store.EnqueueRun(t.Context(), model.RunSpec{RepoID: f.repo.ID, RefKind: model.RefBranch, Ref: "feature", SHA: strings.Repeat(string(rune('a'+index)), 40), Actor: "fixture", Trigger: "push"})
					if err != nil {
						t.Fatal(err)
					}
					if err := f.store.SetRunConcurrencyGroup(t.Context(), run.ID, "shared-state", 2); err != nil {
						t.Fatal(err)
					}
					if mode == "idle" {
						break
					}
					reservation := f.scheduler.admission.reserveRun(run.Run)
					request := admissionRequest{RunID: run.ID, RepoID: f.repo.ID, Group: "shared-state", GroupLimit: 2}
					if index < 2 {
						defer acquireRequestWithinBudget(t, reservation, request)()
					} else {
						ready := make(chan func(), 1)
						go acquireRequestForTest(reservation, request, ready)
						awaitWaiting(t, reservation)
						defer reservation.cancel()
					}
				}
			}
			want := []model.AdmissionGroup{}
			if mode != "nil-observer-list" {
				var err error
				want, err = f.scheduler.ConcurrencyGroups(t.Context())
				if err != nil {
					t.Fatal(err)
				}
			}
			if mode == "holders-and-waiter" && (len(want) != 1 || want[0].Limit != 2 || len(want[0].Holders) != 2 || len(want[0].Waiting) != 1) {
				t.Fatalf("native holder/waiter fixture not exercised: %+v", want)
			}
			server := resultServer(t, control, api.Actor{Identity: "reader"})
			result := resultCall(t, server, "concurrency_groups", map[string]any{})
			validateResult(t, resultSchemas(t, server)["concurrency_groups"], result)
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(result.Structured, &fields); err != nil {
				t.Fatal(err)
			}
			var groups []model.AdmissionGroup
			if err := json.Unmarshal(fields["groups"], &groups); err != nil || groups == nil {
				t.Fatalf("missing array: %s (%v)", result.Structured, err)
			}
			gotJSON, err := json.Marshal(groups)
			if err != nil {
				t.Fatal(err)
			}
			wantJSON, err := json.Marshal(want)
			if err != nil {
				t.Fatal(err)
			}
			if string(gotJSON) != string(wantJSON) {
				t.Fatalf("group/holder/waiter state changed: got=%+v want=%+v", groups, want)
			}
			if len(result.Content) != 1 || result.Content[0].Text != string(result.Structured) {
				t.Fatalf("text/structured mismatch: %+v", result)
			}
		})
	}
}

type concurrencyResultObserver struct{ err error }

func (*concurrencyResultObserver) AdmissionSnapshot(int64) *model.AdmissionObservation { return nil }
func (o *concurrencyResultObserver) ConcurrencyGroups(context.Context) ([]model.AdmissionGroup, error) {
	return nil, o.err
}

func TestConcurrencyGroupsMCPRefusals(t *testing.T) {
	f := newControlFixture(t)
	control := f.api(t)
	control.admission = &concurrencyResultObserver{}
	actor := api.Actor{Identity: "reader"}
	if _, err := control.CallTool(t.Context(), api.Actor{Identity: "scoped", Scope: []string{"origin"}}, "concurrency_groups", json.RawMessage(`{}`)); !errors.Is(err, ErrForbidden) {
		t.Fatalf("scope check lost: %v", err)
	}
	if _, err := control.CallTool(t.Context(), actor, "concurrency_groups", json.RawMessage(`{"unknown":true}`)); err == nil {
		t.Fatal("unknown argument accepted")
	}
	control.admission = nil
	if _, err := control.CallTool(t.Context(), actor, "concurrency_groups", json.RawMessage(`{}`)); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("unavailable observer accepted: %v", err)
	}
	failure := errors.New("observer unavailable")
	control.admission = &concurrencyResultObserver{err: failure}
	if _, err := control.CallTool(t.Context(), actor, "concurrency_groups", json.RawMessage(`{}`)); !errors.Is(err, failure) {
		t.Fatalf("observer failure swallowed: %v", err)
	}
}
