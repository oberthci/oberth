package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
)

// Follow-ups from the first production bulk prunes (#1500).

func (f *pruneFixture) propagator(t *testing.T) *BranchDeletionPropagator {
	t.Helper()
	propagator, err := NewBranchDeletionPropagator(f.store, f.cache, f.store, func(context.Context) error { return nil }, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	return propagator
}

func (f *pruneFixture) pruneMerged(t *testing.T) string {
	t.Helper()
	selector := map[string]any{"repo": f.repo.Name, "merged_into": "main", "exclude_prefixes": []string{"ci/"}}
	plan, err := f.prune(t, pruneAdmin, selector)
	if err != nil {
		t.Fatal(err)
	}
	selector["confirm"], selector["plan_id"] = true, plan.PlanID
	executed, err := f.prune(t, pruneAdmin, selector)
	if err != nil || executed.Deleted == 0 {
		t.Fatalf("prune = %+v, %v", executed, err)
	}
	return executed.BatchID
}

func (f *pruneFixture) resolve(t *testing.T, arguments map[string]any) (BranchDeletionResolveResponse, error) {
	t.Helper()
	raw, err := json.Marshal(arguments)
	if err != nil {
		t.Fatal(err)
	}
	value, err := f.service.CallTool(t.Context(), pruneAdmin, "branch_deletion_resolve", raw)
	response, _ := value.(BranchDeletionResolveResponse)
	return response, err
}

func recordsByBranch(records []BranchDeletionView) map[string]BranchDeletionView {
	byBranch := make(map[string]BranchDeletionView, len(records))
	for _, record := range records {
		byBranch[record.Branch] = record
	}
	return byBranch
}

// An archived upstream refuses every push. The first refusal is final: the
// record is skipped with an "upstream archived" reason line after one
// attempt, and the mirror deletion is kept instead of being restored after
// six retries.
func TestArchivedUpstreamKeepsMirrorDeletion(t *testing.T) {
	f := newPruneFixture(t)
	batch := f.pruneMerged(t)
	hook := filepath.Join(f.upstream, "hooks", "pre-receive")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\necho 'Forgejo: Repo \"org/repo\" is archived.' >&2\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if finished, err := f.propagator(t).Propagate(t.Context()); err != nil || finished != 3 {
		t.Fatalf("propagate = %d, %v", finished, err)
	}
	for _, record := range f.deletions(t, map[string]any{"batch": batch}) {
		if record.Status != "skipped" || record.Attempts != 1 || !strings.HasPrefix(record.Error, "upstream archived") {
			t.Fatalf("record = %+v", record)
		}
	}
	if got := f.git(t, f.mirror, "for-each-ref", "refs/heads/feature/a"); got != "" {
		t.Fatalf("archived upstream restored the mirror branch: %s", got)
	}
	if got := f.git(t, f.upstream, "rev-parse", "refs/heads/feature/a"); got != f.initial {
		t.Fatalf("upstream changed: %s", got)
	}
}

// Failed records end: by an operator with ids, by the tool without ids when
// the branch is gone from the mirror, automatically when a later deletion of
// the same branch propagates, and by the propagator's sweep. Counts are
// global, not per page.
func TestFailedBranchDeletionsResolve(t *testing.T) {
	f := newPruneFixture(t)
	batch := f.pruneMerged(t)
	// Someone moves feature/b and feature/c on the forge: their deletions
	// are refused, fail, and restore the mirror branches.
	f.git(t, f.upstream, "update-ref", "refs/heads/feature/b", f.unmerged)
	f.git(t, f.upstream, "update-ref", "refs/heads/feature/c", f.unmerged)
	if _, err := f.propagator(t).Propagate(t.Context()); err != nil {
		t.Fatal(err)
	}
	first := recordsByBranch(f.deletions(t, map[string]any{"batch": batch}))
	if first["feature/a"].Status != "propagated" || first["feature/b"].Status != "failed" || first["feature/c"].Status != "failed" {
		t.Fatalf("records = %+v", first)
	}

	raw, _ := json.Marshal(map[string]any{"repo": f.repo.Name, "limit": 1})
	value, err := f.service.CallTool(t.Context(), pruneAdmin, "branch_deletion_list", raw)
	if err != nil {
		t.Fatal(err)
	}
	listed := value.(map[string]any)
	if counts := listed["counts"].(map[string]int); counts["failed"] != 2 || counts["propagated"] != 1 {
		t.Fatalf("counts = %v", counts)
	}
	if page := listed["page_counts"].(map[string]int); len(listed["deletions"].([]BranchDeletionView)) != 1 || page["failed"]+page["propagated"] != 1 {
		t.Fatalf("page counts = %v", page)
	}

	// Without ids, branches still on the mirror are kept.
	if _, err := f.resolve(t, map[string]any{"repo": f.repo.Name}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("missing reason = %v", err)
	}
	moot, err := f.resolve(t, map[string]any{"repo": f.repo.Name, "reason": "cleanup"})
	if err != nil || len(moot.Resolved) != 0 || len(moot.Kept) != 2 {
		t.Fatalf("resolve without ids = %+v, %v", moot, err)
	}
	if _, err := f.service.CallTool(t.Context(), api.Actor{Identity: "dev@host"}, "branch_deletion_resolve", json.RawMessage(`{"repo":"x","reason":"y"}`)); !errors.Is(err, ErrForbidden) {
		t.Fatalf("non-admin resolve = %v", err)
	}

	// An operator resolves feature/b by id; refs stay as they are.
	byID, err := f.resolve(t, map[string]any{"repo": f.repo.Name, "ids": []string{first["feature/b"].ID, first["feature/a"].ID}, "reason": "moved on the forge on purpose"})
	if err != nil || strings.Join(byID.Resolved, ",") != first["feature/b"].ID {
		t.Fatalf("resolve by id = %+v, %v", byID, err)
	}
	if got := f.git(t, f.mirror, "rev-parse", "refs/heads/feature/b"); got != f.initial {
		t.Fatalf("resolve changed the mirror: %s", got)
	}

	// feature/c goes back on the forge and is deleted again: the propagated
	// deletion resolves the older failed record.
	f.git(t, f.upstream, "update-ref", "refs/heads/feature/c", f.initial)
	again := map[string]any{"repo": f.repo.Name, "branches": []map[string]string{{"branch": "feature/c", "expected_sha": f.initial}}}
	plan, err := f.prune(t, pruneAdmin, again)
	if err != nil {
		t.Fatal(err)
	}
	again["confirm"], again["plan_id"] = true, plan.PlanID
	if _, err := f.prune(t, pruneAdmin, again); err != nil {
		t.Fatal(err)
	}
	if _, err := f.propagator(t).Propagate(t.Context()); err != nil {
		t.Fatal(err)
	}
	second := recordsByBranch(f.deletions(t, map[string]any{"batch": batch}))
	if second["feature/b"].Status != "skipped" || !strings.Contains(second["feature/b"].Error, "resolved by admin@host: moved on the forge on purpose") {
		t.Fatalf("feature/b = %+v", second["feature/b"])
	}
	if second["feature/c"].Status != "skipped" || !strings.Contains(second["feature/c"].Error, "later deletion") {
		t.Fatalf("feature/c = %+v", second["feature/c"])
	}

	// A failed record whose branch is gone from the mirror: the tool without
	// ids resolves it, and so does the propagator's hourly sweep.
	for _, branch := range []string{"gone/tool", "gone/sweep"} {
		batchID := "prune-" + strings.ReplaceAll(branch, "/", "-")
		if err := f.store.QueueBranchPrune(t.Context(), batchID, f.repo.ID, pruneAdmin.Identity, []model.BranchPruneEntry{{Branch: branch, SHA: f.initial}}); err != nil {
			t.Fatal(err)
		}
		if err := f.store.UpdateBranchDeletions(t.Context(), []model.BranchDeletionUpdate{{ID: batchID + ":0", Status: model.BranchDeletionFailed, Attempts: 1, Error: "refused"}}); err != nil {
			t.Fatal(err)
		}
		if branch == "gone/tool" {
			gone, err := f.resolve(t, map[string]any{"repo": f.repo.Name, "reason": "cleanup"})
			if err != nil || strings.Join(gone.Resolved, ",") != batchID+":0" {
				t.Fatalf("resolve absent = %+v, %v", gone, err)
			}
		}
	}
	if _, err := f.propagator(t).Propagate(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, branch := range []string{"gone/tool", "gone/sweep"} {
		records := f.deletions(t, map[string]any{"batch": "prune-" + strings.ReplaceAll(branch, "/", "-")})
		if len(records) != 1 || records[0].Status != "skipped" || !strings.Contains(records[0].Error, absentBranchReason) {
			t.Fatalf("%s = %+v", branch, records)
		}
	}
	actions, _, err := f.store.ListAuditActions(t.Context(), model.AuditListFilter{ActionPrefix: "branch.deletion.resolve", Limit: 50})
	if err != nil || len(actions) != 3 {
		t.Fatalf("resolve audit = %+v, %v", actions, err)
	}
}

// Every candidate gets a class; report lists the kept ones without changing
// the selection or its plan ID, and cannot confirm.
func TestBranchPlanClassifiesEveryCandidate(t *testing.T) {
	f := newPruneFixture(t)
	selector := map[string]any{"repo": f.repo.Name, "merged_into": "main", "exclude_prefixes": []string{"ci/"}}
	plan, err := f.prune(t, pruneAdmin, selector)
	if err != nil {
		t.Fatal(err)
	}
	selector["report"] = true
	report, err := f.prune(t, pruneAdmin, selector)
	if err != nil {
		t.Fatal(err)
	}
	if report.PlanID != plan.PlanID || plan.Kept != nil || strings.Join(planBranches(report.Kept), ",") != "feature/unmerged" {
		t.Fatalf("report = %+v", report)
	}
	if report.Kept[0].Class != branchClassUnique || report.Branches[0].Class != branchClassMerged {
		t.Fatalf("classes = %+v / %+v", report.Kept[0], report.Branches[0])
	}
	if report.Classes[branchClassMerged] != 3 || report.Classes[branchClassUnique] != 1 || report.TotalCandidates != 4 {
		t.Fatalf("class counts = %v total=%d", report.Classes, report.TotalCandidates)
	}
	selector["confirm"], selector["plan_id"] = true, plan.PlanID
	if _, err := f.prune(t, pruneAdmin, selector); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("report confirm = %v", err)
	}
	if got := f.git(t, f.mirror, "rev-parse", "refs/heads/feature/a"); got != f.initial {
		t.Fatalf("report deleted feature/a: %s", got)
	}
}
