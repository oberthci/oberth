package contracttest

import (
	"context"
	"github.com/oberthci/oberth/internal/publicerr"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/stateport"
)

type AdministrationPort interface {
	ScopedIssue(context.Context, stateport.IssueScope, int64) (model.Issue, error)
	ScopedIssues(context.Context, stateport.IssueScope, model.IssueFilter) (stateport.IssuePage, error)
	ImportIssues(context.Context, stateport.Action, stateport.IssueImport) (stateport.IssueImportResult, error)
	CreateIssue(context.Context, stateport.Action, model.Issue) (model.Issue, error)
	UpdateIssue(context.Context, stateport.Action, model.IssuePatch) (model.Issue, error)
	ChangeGrant(context.Context, stateport.Action, stateport.GrantChange) (stateport.AuthoritySnapshot, error)
	Grants(context.Context, []string) (stateport.AuthoritySnapshot, error)
	RecordAdministration(context.Context, stateport.Action, model.AuditAction) error
	AdministrativeAudit(context.Context, []string, stateport.AdministrativeFilter) ([]stateport.AuditRecord, error)
	RegisterUpstreamBinding(context.Context, stateport.Action, stateport.Upstream) error
	UpstreamBindings(context.Context) ([]stateport.Upstream, error)
	Events(context.Context, stateport.EventFilter) (model.EventPage, error)
	Changes() <-chan struct{}
}

type AdministrationFixture struct {
	Port       AdministrationPort
	Repository func(*testing.T, string)
	Now        func() time.Time
}

func AdministrationSuite(t *testing.T, factory func(*testing.T) AdministrationFixture) {
	for name, check := range map[string]func(*testing.T, AdministrationFixture){
		"invalid_grant":          checkInvalidGrant,
		"dismiss_pending_only":   checkPendingGrantDismissal,
		"issue_read_scope":       checkIssueReadScope,
		"issue_write_scope":      checkIssueWriteScope,
		"issue_list_counts":      checkIssueListCounts,
		"issue_list_paging":      checkIssueListPaging,
		"grant_kinds_snapshot":   checkGrantKinds,
		"audit_scope_filters":    checkAdministrativeAudit,
		"audit_actor":            checkAdministrativeActor,
		"upstream_binding":       checkUpstreamBinding,
		"reserved_global":        checkReservedGlobalUpstream,
		"late_grant_rollback":    checkGrantRollback,
		"late_audit_rollback":    checkAdministrativeRollback,
		"late_upstream_rollback": checkUpstreamRollback,
	} {
		t.Run(name, func(t *testing.T) { check(t, factory(t)) })
	}
}

const administrativeRepo = "origin/team/repo"

func administrativeAction(key string) stateport.Action {
	return stateport.Action{Actor: "operator", Reason: "approved reason", Events: []stateport.EventWrite{{Dedupe: key, Event: model.AgentEvent{Kind: string(model.EventRunUpdated), Repo: administrativeRepo, Summary: "authority recorded"}}}}
}

func checkGrantKinds(t *testing.T, f AdministrationFixture) {
	ctx := t.Context()
	f.Repository(t, "origin/team/hidden")
	initial := administrationState(t, f)
	kinds := []stateport.GrantKind{stateport.GrantStorePath, stateport.GrantSecret, stateport.GrantConfigMap, stateport.GrantImagePull, stateport.GrantPVC, stateport.GrantDevice, stateport.GrantAudience, stateport.GrantCapability}
	for _, kind := range kinds {
		g := administrativeGrant(kind)
		out, err := f.Port.ChangeGrant(ctx, administrativeAction(string(kind)), g)
		mustRun(t, err)
		if out.Revision != initial.Authority.Revision+int64(len(out.Grants)) {
			t.Fatal("generation or full snapshot lost", out)
		}
	}
	out := administrationState(t, f)
	if len(out.Authority.Grants) != len(kinds) || len(out.Audits) != len(initial.Audits)+len(kinds) || len(out.Events.Events) != len(initial.Events.Events)+len(kinds) {
		t.Fatal("grant fact/audit/event split", out)
	}
	g := out.Authority.Grants[0].Clone()
	g.Repository = "origin/team/hidden"
	all, err := f.Port.ChangeGrant(ctx, administrativeAction("hidden-grant"), g)
	mustRun(t, err)
	if len(all.Grants) != len(kinds)+1 {
		t.Fatal("change returned a partial generation", all)
	}
	visible, err := f.Port.Grants(ctx, []string{administrativeRepo})
	mustRun(t, err)
	if len(visible.Grants) != len(kinds) {
		t.Fatal("grant scope lost", visible)
	}
	for _, v := range visible.Grants {
		if v.Repository != administrativeRepo {
			t.Fatal("foreign grant", v)
		}
	}
	empty, err := f.Port.Grants(ctx, []string{})
	mustRun(t, err)
	if len(empty.Grants) != 0 {
		t.Fatal("empty scope grants authority", empty)
	}
	checkGrantRevokeAndCopies(t, f, out.Authority.Grants[0])
}

func checkGrantRevokeAndCopies(t *testing.T, f AdministrationFixture, g stateport.GrantChange) {
	g.Revoke = true
	first, err := f.Port.ChangeGrant(t.Context(), administrativeAction("revoke"), g)
	mustRun(t, err)
	g.Revoke = false
	second, err := f.Port.ChangeGrant(t.Context(), administrativeAction("approve"), g)
	mustRun(t, err)
	if second.Revision != first.Revision+1 {
		t.Fatal("grant generation lost", first, second)
	}
	for i := range second.Grants {
		second.Grants[i].Capability = []byte("changed")
	}
	stored, err := f.Port.Grants(t.Context(), nil)
	mustRun(t, err)
	if reflect.DeepEqual(second, stored) {
		t.Fatal("snapshot aliases stored grants")
	}
}

func administrativeRecord(repo, action string) model.AuditAction {
	return model.AuditAction{Actor: "operator", Action: action, ResourceType: "repository", ResourceID: repo, Details: `{"approved":true}`}
}

func checkAdministrativeActor(t *testing.T, f AdministrationFixture) {
	before := administrationState(t, f)
	a := administrativeAction("forged-audit")
	v := administrativeRecord(administrativeRepo, "forged")
	v.Actor = "another caller"
	if err := f.Port.RecordAdministration(t.Context(), a, v); err == nil {
		t.Fatal("audit actor was replaced")
	}
	if !reflect.DeepEqual(before, administrationState(t, f)) {
		t.Fatal("invalid audit changed facts")
	}
}

func checkAdministrativeAudit(t *testing.T, f AdministrationFixture) {
	ctx := t.Context()
	f.Repository(t, "origin/team/hidden")
	mustRun(t, f.Port.RecordAdministration(ctx, administrativeAction("visible-audit"), administrativeRecord(administrativeRepo, "visible")))
	mustRun(t, f.Port.RecordAdministration(ctx, administrativeAction("hidden-audit"), administrativeRecord("origin/team/hidden", "hidden")))
	q := stateport.AdministrativeFilter{Limit: 1}
	rows, err := f.Port.AdministrativeAudit(ctx, []string{administrativeRepo}, q)
	mustRun(t, err)
	if len(rows) != 1 || rows[0].Action != "visible" {
		t.Fatal("scope applied after limit", rows)
	}
	v := rows[0]
	if v.SHA256 != stateport.AuditDigest(stateport.DefaultProtocol().AuditDomain, v.PreviousSHA256, v.AuditAction) || v.PreviousSHA256 == "" {
		t.Fatal("audit chain lost", v)
	}
	q.Before = v.ID
	rows, err = f.Port.AdministrativeAudit(ctx, []string{administrativeRepo}, q)
	mustRun(t, err)
	if len(rows) > 0 && rows[0].ID >= v.ID {
		t.Fatal("exclusive audit cursor lost", rows)
	}
	q = stateport.AdministrativeFilter{Limit: 200, Action: "visible", Actor: "operator", Since: f.Now().Format(time.RFC3339Nano), Until: f.Now().Format(time.RFC3339Nano)}
	rows, err = f.Port.AdministrativeAudit(ctx, nil, q)
	mustRun(t, err)
	if len(rows) != 1 {
		t.Fatal("audit exact filters lost", rows)
	}
	q.Until = f.Now().Add(-time.Nanosecond).Format(time.RFC3339Nano)
	q.Since = ""
	rows, err = f.Port.AdministrativeAudit(ctx, nil, q)
	mustRun(t, err)
	if len(rows) != 0 {
		t.Fatal("audit until ignored", rows)
	}
	q.Limit = 201
	if _, err = f.Port.AdministrativeAudit(ctx, nil, q); err == nil {
		t.Fatal("audit limit exceeded")
	}
}

func checkUpstreamBinding(t *testing.T, f AdministrationFixture) {
	u := stateport.Upstream{Name: "extra", Org: "extra-team", Kind: "git", BaseURL: "https://extra.oberth.ci", KeyName: "source"}
	a := administrativeAction("upstream-recorded")
	before := administrationState(t, f)
	mustRun(t, f.Port.RegisterUpstreamBinding(t.Context(), a, u))
	after := administrationState(t, f)
	if len(after.Upstreams) != len(before.Upstreams)+1 || len(after.Audits) != len(before.Audits)+1 || len(after.Events.Events) != len(before.Events.Events)+1 {
		t.Fatal("binding fact/audit/events split", after)
	}
	mustRun(t, f.Port.RegisterUpstreamBinding(t.Context(), a, u))
	if !reflect.DeepEqual(after, administrationState(t, f)) {
		t.Fatal("binding replay changed facts")
	}
	u.KeyName = "replacement"
	if err := f.Port.RegisterUpstreamBinding(t.Context(), a, u); err == nil {
		t.Fatal("binding replacement accepted")
	}
}

type administrationSnapshot struct {
	Authority stateport.AuthoritySnapshot
	Audits    []stateport.AuditRecord
	Events    model.EventPage
	Upstreams []stateport.Upstream
}

func administrationState(t *testing.T, f AdministrationFixture) administrationSnapshot {
	t.Helper()
	g, err := f.Port.Grants(t.Context(), nil)
	mustRun(t, err)
	a, err := f.Port.AdministrativeAudit(t.Context(), nil, stateport.AdministrativeFilter{Limit: 200})
	mustRun(t, err)
	e, err := f.Port.Events(t.Context(), stateport.EventFilter{Limit: 500})
	mustRun(t, err)
	u, err := f.Port.UpstreamBindings(t.Context())
	mustRun(t, err)
	return administrationSnapshot{g, a, e, u}
}

func conflictingAdministrativeAction(t *testing.T, f AdministrationFixture) stateport.Action {
	t.Helper()
	a := administrativeAction("retained-event")
	mustRun(t, f.Port.RecordAdministration(t.Context(), a, administrativeRecord(administrativeRepo, "retained")))
	conflict := a.Events[0]
	conflict.Event.Summary = "conflicting body"
	a.Events = append(administrativeAction("new-staged-event").Events, conflict)
	return a
}

func assertAdministrativeRollback(t *testing.T, f AdministrationFixture, run func(stateport.Action) error) {
	t.Helper()
	a := conflictingAdministrativeAction(t, f)
	before := administrationState(t, f)
	changed := f.Port.Changes()
	if err := run(a); err == nil {
		t.Fatal("late event conflict accepted")
	}
	if !reflect.DeepEqual(before, administrationState(t, f)) {
		t.Fatal("late conflict committed a partial fact, audit or event")
	}
	unchangedSignal(t, changed)
}

func checkGrantRollback(t *testing.T, f AdministrationFixture) {
	f.Repository(t, "origin/team/hidden")
	g := stateport.GrantChange{Kind: stateport.GrantPVC, Repository: administrativeRepo, Path: "claim", Tier: "ci"}
	assertAdministrativeRollback(t, f, func(a stateport.Action) error {
		v, err := f.Port.ChangeGrant(t.Context(), a, g)
		if v.Revision != 0 {
			t.Fatal("returned uncommitted snapshot", v)
		}
		return err
	})
	g.Repository = "origin/team/hidden"
	_, err := f.Port.ChangeGrant(t.Context(), administrativeAction("claim-after-rollback"), g)
	mustRun(t, err)
	g.Repository = administrativeRepo
	if _, err = f.Port.ChangeGrant(t.Context(), administrativeAction("foreign-claim"), g); err == nil {
		t.Fatal("PVC owner changed")
	}
}

func checkAdministrativeRollback(t *testing.T, f AdministrationFixture) {
	assertAdministrativeRollback(t, f, func(a stateport.Action) error {
		return f.Port.RecordAdministration(t.Context(), a, administrativeRecord(administrativeRepo, "not-committed"))
	})
}

func checkUpstreamRollback(t *testing.T, f AdministrationFixture) {
	assertAdministrativeRollback(t, f, func(a stateport.Action) error {
		return f.Port.RegisterUpstreamBinding(t.Context(), a, stateport.Upstream{Name: "rollback", Org: "rollback-team", Kind: "git", BaseURL: "https://rollback.oberth.ci", KeyName: "source"})
	})
}

func checkInvalidGrant(t *testing.T, f AdministrationFixture) {
	before := administrationState(t, f)
	for _, kind := range []stateport.GrantKind{"unknown", "release_identity"} {
		if _, err := f.Port.ChangeGrant(t.Context(), administrativeAction("invalid-kind"), stateport.GrantChange{Kind: kind, Repository: administrativeRepo}); err == nil {
			t.Fatal("unsupported grant kind accepted", kind)
		}
	}
	if !reflect.DeepEqual(before, administrationState(t, f)) {
		t.Fatal("invalid grant changed facts")
	}
}

func administrativeGrant(kind stateport.GrantKind) stateport.GrantChange {
	g := stateport.GrantChange{Kind: kind, Repository: administrativeRepo, File: ".oberth/build.yaml", Step: "build", Path: "credential", Capability: []byte(`{"namespace":"ci"}`)}
	if kind == stateport.GrantPVC {
		g.File, g.Step, g.Tier = "", "", "ci"
	}
	return g
}

func checkPendingGrantDismissal(t *testing.T, f AdministrationFixture) {
	pending := administrativeGrant(stateport.GrantStorePath)
	pending.Path = "pending/key"
	pending.Dismiss = true
	for _, key := range []string{"dismiss-pending", "dismiss-pending-again"} {
		_, err := f.Port.ChangeGrant(t.Context(), administrativeAction(key), pending)
		mustRun(t, err)
	}
	for _, revoked := range []bool{false, true} {
		g := administrativeGrant(stateport.GrantStorePath)
		g.Path = map[bool]string{false: "approved/key", true: "revoked/key"}[revoked]
		_, err := f.Port.ChangeGrant(t.Context(), administrativeAction(g.Path+"-approve"), g)
		mustRun(t, err)
		if revoked {
			g.Revoke = true
			_, err = f.Port.ChangeGrant(t.Context(), administrativeAction(g.Path+"-revoke"), g)
			mustRun(t, err)
		}
		before := administrationState(t, f)
		changes := f.Port.Changes()
		g.Revoke = false
		g.Dismiss = true
		_, err = f.Port.ChangeGrant(t.Context(), administrativeAction(g.Path+"-dismiss"), g)
		if err == nil {
			t.Fatal("dismiss ended an approved or revoked grant")
		}
		refusal := publicerr.Classify(err)
		if !strings.Contains(strings.ToLower(refusal.Reason()), "pending") || !strings.Contains(refusal.Remedy(), "access revoke") {
			t.Fatal("dismiss refusal lacks its repair", err)
		}
		if len(refusal.Reason()) > 40 || len(refusal.Remedy()) > 120 {
			t.Fatal("dismiss refusal exceeds its bounds")
		}
		if after := administrationState(t, f); !reflect.DeepEqual(before, after) {
			t.Fatal("refused dismissal changed authority, audit or events")
		}
		select {
		case <-changes:
			t.Fatal("refused dismissal notified a change")
		default:
		}
	}
}

func checkReservedGlobalUpstream(t *testing.T, f AdministrationFixture) {
	before := administrationState(t, f)
	u := stateport.Upstream{Name: "global", Org: "global-team", Kind: "git", BaseURL: "https://global.oberth.ci", KeyName: "source"}
	err := f.Port.RegisterUpstreamBinding(t.Context(), administrativeAction("reserved-global"), u)
	if err == nil || publicerr.Classify(err).Code() != publicerr.InvalidInput {
		t.Fatal("global registered as an ordinary upstream", err)
	}
	if !reflect.DeepEqual(before, administrationState(t, f)) {
		t.Fatal("reserved name wrote state")
	}
}
