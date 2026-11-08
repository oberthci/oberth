package service

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
)

// seedStray gives the prune fixture's mirror stray refs: a copy of main, a
// symbolic HEAD pointing at the public main branch, a copy of the unmerged
// branch, a note outside the upstream prefix, a tag and a private ref.
func (f *pruneFixture) seedStray(t *testing.T) {
	t.Helper()
	f.git(t, f.mirror, "update-ref", "refs/remotes/upstream/main", f.initial)
	f.git(t, f.mirror, "update-ref", "refs/remotes/upstream/cx/old", f.unmerged)
	f.git(t, f.mirror, "update-ref", "refs/notes/commits", f.initial)
	f.git(t, f.mirror, "update-ref", "refs/tags/v1", f.initial)
	f.git(t, f.mirror, "update-ref", "refs/oberth/test/private", f.initial)
	f.git(t, f.mirror, "symbolic-ref", "refs/remotes/upstream/HEAD", "refs/heads/main")
}

func (f *pruneFixture) pruneRefs(t *testing.T, actor api.Actor, arguments map[string]any) (RefPruneResponse, error) {
	t.Helper()
	raw, err := json.Marshal(arguments)
	if err != nil {
		t.Fatal(err)
	}
	value, err := f.service.CallTool(t.Context(), actor, "ref_delete_many", raw)
	response, _ := value.(RefPruneResponse)
	return response, err
}

func refNames(entries []RefPlanEntry) []string {
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Ref)
	}
	return names
}

func TestRefDeleteManyPlansConfirmsAndAudits(t *testing.T) {
	f := newPruneFixture(t)
	f.seedStray(t)
	listing, err := f.pruneRefs(t, pruneAdmin, map[string]any{"repo": f.repo.Name})
	if err != nil {
		t.Fatal(err)
	}
	if !listing.DryRun || listing.Count != 0 || listing.TotalRefs != 4 || strings.Join(refNames(listing.Kept), ",") != "refs/notes/commits,refs/remotes/upstream/HEAD,refs/remotes/upstream/cx/old,refs/remotes/upstream/main" ||
		listing.Classes[refClassDefaultBranch] != 3 || listing.Classes[refClassUnreachable] != 1 || listing.DefaultBranch != "main" {
		t.Fatalf("listing = %+v", listing)
	}
	if _, err := f.pruneRefs(t, pruneAdmin, map[string]any{"repo": f.repo.Name, "confirm": true, "plan_id": listing.PlanID}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("confirm without a selection = %v", err)
	}
	selector := map[string]any{"repo": f.repo.Name, "prefixes": []string{"refs/remotes/upstream/"}}
	plan, err := f.pruneRefs(t, pruneAdmin, selector)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(refNames(plan.Refs), ","); got != "refs/remotes/upstream/HEAD,refs/remotes/upstream/main" || plan.Count != 2 {
		t.Fatalf("plan refs = %s (%+v)", got, plan)
	}
	if got := strings.Join(refNames(plan.Kept), ","); got != "refs/notes/commits,refs/remotes/upstream/cx/old" {
		t.Fatalf("plan kept = %s", got)
	}
	if plan.Refs[0].Symref != "refs/heads/main" || !plan.Refs[0].Reachable || plan.Kept[1].Class != refClassUnreachable || plan.Kept[1].Reachable {
		t.Fatalf("plan entries = %+v / %+v", plan.Refs, plan.Kept)
	}
	if _, err := f.pruneRefs(t, pruneAdmin, map[string]any{"repo": f.repo.Name, "prefixes": []string{"refs/remotes/upstream/"}, "confirm": true}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("confirm without plan_id = %v", err)
	}
	unreachable := map[string]any{"repo": f.repo.Name, "prefixes": []string{"refs/remotes/upstream/"}, "allow_unreachable": true}
	if wider, err := f.pruneRefs(t, pruneAdmin, unreachable); err != nil || wider.Count != 3 || wider.PlanID == plan.PlanID {
		t.Fatalf("allow_unreachable plan = %+v, %v", wider, err)
	}

	confirm := map[string]any{"repo": f.repo.Name, "prefixes": []string{"refs/remotes/upstream/"}, "confirm": true, "plan_id": plan.PlanID}
	executed, err := f.pruneRefs(t, pruneAdmin, confirm)
	if err != nil {
		t.Fatal(err)
	}
	if executed.Deleted != 2 || len(executed.Skipped) != 0 {
		t.Fatalf("executed = %+v", executed)
	}
	refs := f.git(t, f.mirror, "for-each-ref", "--format=%(refname) %(objectname)")
	for _, kept := range []string{"refs/heads/main " + f.initial, "refs/heads/feature/unmerged " + f.unmerged, "refs/tags/v1 " + f.initial,
		"refs/oberth/test/private " + f.initial, "refs/notes/commits " + f.initial, "refs/remotes/upstream/cx/old " + f.unmerged} {
		if !strings.Contains(refs+"\n", kept+"\n") {
			t.Errorf("ref %q changed:\n%s", kept, refs)
		}
	}
	if strings.Contains(refs, "refs/remotes/upstream/main ") || strings.Contains(refs, "refs/remotes/upstream/HEAD ") {
		t.Fatalf("stray refs survived:\n%s", refs)
	}
	if got := f.git(t, f.upstream, "for-each-ref", "--format=%(refname)", "refs/heads/"); !strings.Contains(got, "refs/heads/main") {
		t.Fatalf("upstream heads = %s", got)
	}
	actions, _, err := f.store.ListAuditActions(t.Context(), model.AuditListFilter{ActionPrefix: "ref.prune", Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, action := range actions {
		counts[action.Action]++
		if action.Actor != pruneAdmin.Identity || !strings.Contains(action.Details, plan.PlanID) || !strings.Contains(action.Details, "refs/remotes/upstream/main "+f.initial) {
			t.Fatalf("audit = %+v", action)
		}
		if action.Action == "ref.prune.succeeded" && !strings.Contains(action.Details, `"deleted":2`) {
			t.Fatalf("outcome audit = %+v", action)
		}
	}
	if counts["ref.prune.intent"] != 1 || counts["ref.prune.succeeded"] != 1 {
		t.Fatalf("audit actions = %v", counts)
	}
}

func TestRefDeleteManyRefusesStalePlansAndProtectedRefs(t *testing.T) {
	f := newPruneFixture(t)
	f.seedStray(t)
	for _, actor := range []api.Actor{{Identity: "dev@host"}, {Identity: "scoped@host", Admin: true, Scope: []string{"codeberg"}}} {
		if _, err := f.pruneRefs(t, actor, map[string]any{"repo": f.repo.Name}); !errors.Is(err, ErrForbidden) {
			t.Fatalf("%s = %v", actor.Identity, err)
		}
	}
	for _, prefix := range []string{"refs/heads/", "refs/tags/", "refs/oberth/", "refs/replace/", "refs/heads", "heads/"} {
		if _, err := f.pruneRefs(t, pruneAdmin, map[string]any{"repo": f.repo.Name, "prefixes": []string{prefix}}); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("prefix %q = %v", prefix, err)
		}
	}

	explicit := map[string]any{"repo": f.repo.Name, "refs": []map[string]string{
		{"ref": "refs/heads/main", "expected_sha": f.initial},
		{"ref": "refs/tags/v1", "expected_sha": f.initial},
		{"ref": "refs/oberth/test/private", "expected_sha": f.initial},
		{"ref": "refs/remotes/upstream/cx/old", "expected_sha": f.unmerged},
		{"ref": "refs/remotes/upstream/gone", "expected_sha": f.initial},
		{"ref": "refs/notes/commits", "expected_sha": f.unmerged},
		{"ref": "refs/remotes/upstream/main", "expected_sha": f.initial},
	}}
	plan, err := f.pruneRefs(t, pruneAdmin, explicit)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(refNames(plan.Refs), ","); got != "refs/remotes/upstream/main" {
		t.Fatalf("explicit plan = %s (%+v)", got, plan)
	}
	reasons := map[string]string{}
	for _, refusal := range plan.Refused {
		reasons[refusal.Ref] = refusal.Reason
	}
	for ref, want := range map[string]string{
		"refs/heads/main": "not a stray ref", "refs/tags/v1": "not a stray ref", "refs/oberth/test/private": "not a stray ref",
		"refs/remotes/upstream/cx/old": "allow_unreachable", "refs/remotes/upstream/gone": "does not exist", "refs/notes/commits": "ref moved",
	} {
		if !strings.Contains(reasons[ref], want) {
			t.Errorf("refusal of %s = %q, want %q", ref, reasons[ref], want)
		}
	}

	// A ref moving after the dry run makes the plan stale; nothing changes.
	f.git(t, f.mirror, "update-ref", "refs/remotes/upstream/main", f.unmerged)
	explicit["confirm"], explicit["plan_id"] = true, plan.PlanID
	if _, err := f.pruneRefs(t, pruneAdmin, explicit); !errors.Is(err, ErrInvalidInput) || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("stale confirm = %v", err)
	}
	if got := f.git(t, f.mirror, "rev-parse", "refs/remotes/upstream/main"); got != f.unmerged {
		t.Fatalf("moved ref = %s", got)
	}
	if got := f.git(t, f.mirror, "rev-parse", "refs/heads/main"); got != f.initial {
		t.Fatalf("main = %s", got)
	}
	if actions, _, err := f.store.ListAuditActions(t.Context(), model.AuditListFilter{ActionPrefix: "ref.prune", Limit: 50}); err != nil || len(actions) != 0 {
		t.Fatalf("audit after refusals = %+v, %v", actions, err)
	}
}
