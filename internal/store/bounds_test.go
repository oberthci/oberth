package store

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stateport"
)

type planCase struct {
	id   string
	plan admissioncontract.Plan
}

func acceptPlan(t *testing.T, s *Store, input stateport.Acceptance, bound planCase) error {
	t.Helper()
	view := input.Prepared.View()
	view.RunID = bound.id
	view.Plan = bound.plan
	sealed, err := admissioncontract.Seal(view)
	requireOK(t, err)
	input.Prepared = sealed
	input.Request.ReceiveID = bound.id
	_, err = acceptForTest(t.Context(), s, input)
	return err
}

func TestPlanEntryAndByteBoundsThroughAcceptance(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	_, input := accepted(t, s, "base-plan")
	entries := make([]admissioncontract.PlanEntry, admissioncontract.MaxPlanEntries)
	for i := range entries {
		entries[i] = admissioncontract.PlanEntry{Path: fmt.Sprintf("step-%d", i)}
	}
	requireOK(t, acceptPlan(t, s, input, planCase{"entries-at-limit", admissioncontract.Plan{Entries: entries, Exact: true}}))
	entries = append(entries, admissioncontract.PlanEntry{Path: "one-too-many"})
	requireCode(t, acceptPlan(t, s, input, planCase{"entries-over-limit", admissioncontract.Plan{Entries: entries, Exact: true}}), publicerr.InvalidInput)
	plan := admissioncontract.Plan{Entries: []admissioncontract.PlanEntry{{Path: "step"}}, Exact: true}
	plan.Entries[0].Name = strings.Repeat("x", admissioncontract.MaxPlanBytes-len(encode(plan)))
	if len(encode(plan)) != admissioncontract.MaxPlanBytes {
		t.Fatal("bad exact-size fixture")
	}
	requireOK(t, acceptPlan(t, s, input, planCase{"bytes-at-limit", plan}))
	plan.Entries[0].Name += "x"
	requireCode(t, acceptPlan(t, s, input, planCase{"bytes-over-limit", plan}), publicerr.InvalidInput)
}

func TestCapabilityAndKeyScopeBounds(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	requireOK(t, s.RegisterUpstream(t.Context(), stateport.Action{Actor: "operator"}, stateport.Upstream{Name: "origin", Org: "team", Kind: "git", BaseURL: "https://oberth.ci", KeyName: "source"}))
	_, errSetup := s.RegisterRepository(t.Context(), stateport.Action{Actor: "operator"}, "origin/team/repo")
	requireOK(t, errSetup)
	at := []byte(`"` + strings.Repeat("x", (16<<10)-2) + `"`)
	requireOK(t, s.Capability(t.Context(), stateport.Action{Actor: "operator"}, CapabilityGrant{Repository: "origin/team/repo", JSON: at}))
	over := []byte(`"` + strings.Repeat("x", (16<<10)-1) + `"`)
	requireCode(t, s.Capability(t.Context(), stateport.Action{Actor: "operator"}, CapabilityGrant{Repository: "origin/team/repo", JSON: over}), publicerr.InvalidInput)
	scope := make([]string, 64)
	for i := range scope {
		scope[i] = fmt.Sprintf("scope-%d", i)
	}
	key := stateport.Key{Fingerprint: "bounded-key", Identity: "agent", Kind: "agent", ParkOnPush: true, Scope: scope}
	requireOK(t, s.RegisterKey(t.Context(), stateport.Action{Actor: "operator"}, key))
	key.Fingerprint = "too-many-scopes"
	key.Scope = append(scope, "extra")
	requireCode(t, s.RegisterKey(t.Context(), stateport.Action{Actor: "operator"}, key), publicerr.InvalidInput)
}

func TestChangeGrantCapabilitySizeBound(t *testing.T) {
	t.Parallel()
	for _, size := range []int{16 << 10, (16 << 10) + 1} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			s := testStore(t)
			repo := atomicRepository(t, s)
			body := []byte(`"` + strings.Repeat("x", size-2) + `"`)
			grant := stateport.GrantChange{Kind: stateport.GrantCapability, Repository: repo.Canonical, Capability: body}
			before := databaseContents(t, s.readers)
			snapshot, err := s.ChangeGrant(t.Context(), stateport.Action{Actor: "operator"}, grant)
			if size > 16<<10 {
				if databaseContents(t, s.readers) != before {
					t.Error("oversized capability changed grants, revision, audit or events")
				}
				requireCode(t, err, publicerr.InvalidInput)
				if snapshot.Revision != 0 || len(snapshot.Grants) != 0 {
					t.Fatal("refused capability returned authority")
				}
				return
			}
			requireOK(t, err)
			stored, err := s.Grants(t.Context(), nil)
			requireOK(t, err)
			if stored.Revision != 1 || len(stored.Grants) != 1 || !bytes.Equal(stored.Grants[0].Capability, body) {
				t.Fatal("capability at the byte limit was not stored exactly")
			}
		})
	}
}

func TestProtocolAndSnapshotLimits(t *testing.T) {
	t.Parallel()
	for _, size := range []int{128, 129} {
		options := testOptions()
		options.Protocol.SchemaIdentity = strings.Repeat("a", size)
		options.Protocol.AuditDomain = strings.Repeat("b", size)
		s, err := Create(t.Context(), t.TempDir()+"/state.sqlite", options)
		if size == 128 {
			requireOK(t, err)
			requireOK(t, s.Close())
		} else {
			requireCode(t, err, publicerr.InvalidInput)
		}
	}
	s := testStore(t)
	requireOK(t, s.RegisterUpstream(t.Context(), stateport.Action{Actor: "operator"}, stateport.Upstream{Name: "origin", Org: "team", Kind: "git", BaseURL: "https://oberth.ci", KeyName: "source"}))
	var snapshot bytes.Buffer
	_, err := s.Snapshot(t.Context(), backupOptions(s), &snapshot)
	requireOK(t, err)
	infoSize := snapshotDatabaseSize(t, s)
	options := backupOptions(s)
	options.MaxBytes = infoSize
	_, err = s.Snapshot(t.Context(), options, &bytes.Buffer{})
	requireOK(t, err)
	options.MaxBytes = infoSize - 1
	_, err = s.Snapshot(t.Context(), options, &bytes.Buffer{})
	requireCode(t, err, publicerr.InvalidInput)
	options.MaxBytes = stateport.HardMaxBytes
	_, err = s.Snapshot(t.Context(), options, &bytes.Buffer{})
	requireOK(t, err)
	options.MaxBytes++
	_, err = s.Snapshot(t.Context(), options, &bytes.Buffer{})
	requireCode(t, err, publicerr.InvalidInput)
}

func snapshotDatabaseSize(t *testing.T, s *Store) int64 {
	t.Helper()
	var pages, size int64
	requireOK(t, s.readers.QueryRowContext(t.Context(), `PRAGMA page_count`).Scan(&pages))
	requireOK(t, s.readers.QueryRowContext(t.Context(), `PRAGMA page_size`).Scan(&size))
	return pages * size
}

func TestEventBoundsAndExclusiveRestartCursor(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	event := model.AgentEvent{Repo: "origin/team/repo", Kind: string(model.EventRunUpdated), At: s.options.Clock(), Summary: strings.Repeat("x", 512), Data: []byte(`"` + strings.Repeat("x", 8190) + `"`)}
	input := stateport.EventWrite{Event: event, Dedupe: strings.Repeat("d", 512)}
	first, err := eventForTest(t.Context(), s, input)
	requireOK(t, err)
	again, err := eventForTest(t.Context(), s, input)
	requireOK(t, err)
	if again != first {
		t.Fatal("dedupe changed cursor")
	}
	for _, kind := range []string{"summary", "data", "dedupe"} {
		bad := input
		bad.Event = bad.Event.Clone()
		switch kind {
		case "summary":
			bad.Event.Summary += "x"
		case "data":
			bad.Event.Data = []byte(`"` + strings.Repeat("x", 8191) + `"`)
		case "dedupe":
			bad.Dedupe += "x"
		}
		_, err := eventForTest(t.Context(), s, bad)
		requireCode(t, err, publicerr.InvalidInput)
	}
	_, err = s.Events(t.Context(), stateport.EventFilter{Repositories: nil, After: 0, Limit: 500})
	requireOK(t, err)
	_, err = s.Events(t.Context(), stateport.EventFilter{Repositories: nil, After: 0, Limit: 501})
	requireCode(t, err, publicerr.InvalidInput)
	empty, err := s.Events(t.Context(), stateport.EventFilter{Repositories: []string{}, After: 0, Limit: 500})
	requireOK(t, err)
	if len(empty.Events) != 0 || empty.LatestID != 0 {
		t.Fatal("empty scope widened")
	}
	second, err := Open(t.Context(), s.path, s.options)
	requireOK(t, err)
	defer func() { requireOK(t, second.Close()) }()
	input.Dedupe = "second"
	input.Event.Summary = "progress observed"
	input.Event.At = input.Event.At.Add(time.Second)
	id, err := eventForTest(t.Context(), second, input)
	requireOK(t, err)
	page, err := s.Events(t.Context(), stateport.EventFilter{Repositories: []string{event.Repo}, After: first, Limit: 500})
	requireOK(t, err)
	if len(page.Events) != 1 || page.Events[0].ID != id || id <= first {
		t.Fatal("cursor restart/order")
	}
}

func TestReadPageAndReceiveBounds(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	_, input := accepted(t, s, "receive-base")
	view := input.Prepared.View()
	view.RunID = "long-receive"
	var err error
	input.Prepared, err = admissioncontract.Seal(view)
	requireOK(t, err)
	input.Request.ReceiveID = strings.Repeat("a", 512)
	_, err = acceptForTest(t.Context(), s, input)
	requireOK(t, err)
	input.Request.ReceiveID += "a"
	_, err = acceptForTest(t.Context(), s, input)
	requireCode(t, err, publicerr.InvalidInput)
	_, _, err = s.Audit(t.Context(), 0, 200)
	requireOK(t, err)
	_, _, err = s.Audit(t.Context(), 0, 201)
	requireCode(t, err, publicerr.InvalidInput)
	_, err = s.Issues(t.Context(), nil, model.IssueFilter{Limit: 200})
	requireOK(t, err)
	_, err = s.Issues(t.Context(), nil, model.IssueFilter{Limit: 201})
	requireCode(t, err, publicerr.InvalidInput)

}
