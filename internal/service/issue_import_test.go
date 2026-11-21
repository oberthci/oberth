package service_test

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stateport"
)

func importRecord(id int64) model.IssueImportRecord {
	at := time.Date(2021, 2, 3, 4, 5, 6, 123456789, time.FixedZone("offset", 3600))
	return model.IssueImportRecord{ID: id, Upstream: "origin", Title: "imported task", Body: "retained body", State: model.IssueClosed,
		CreatedAt: at, UpdatedAt: at.Add(time.Hour), ClosedAt: new(at.Add(time.Hour)), Author: "source author", Closer: "source closer",
		Kind: model.IssueManual, WorkStatus: model.IssueReview, Occurrences: 2,
		History: []model.IssueHistory{{Kind: "append", At: at, Author: "source author", Text: "first"}, {Kind: "state", At: at.Add(time.Hour), Author: "source closer", State: model.IssueClosed}}}
}
func importArgs(t *testing.T, rows ...model.IssueImportRecord) apicontract.IssueImportArgs {
	t.Helper()
	var lines []string
	for _, r := range rows {
		raw, e := json.Marshal(r)
		if e != nil {
			t.Fatal(e)
		}
		lines = append(lines, string(raw))
	}
	return apicontract.IssueImportArgs{JSONL: countedJSONL(strings.Join(lines, "\n") + "\n")}
}
func importCall(t *testing.T, p *peopleFixture, q apicontract.IssueImportArgs) (stateport.IssueImportResult, error) {
	t.Helper()
	call, err := p.f.s.PrepareIssueImport(t.Context(), adminCaller(), q)
	return execute(t.Context(), call, err)
}

// One service contract exercises the real store and its in-memory implementation.
func TestIssueImportContract(t *testing.T) {
	for _, real := range []bool{false, true} {
		t.Run(fmt.Sprint(real), func(t *testing.T) {
			p := people(t)
			if !real {
				p.f.c.People.Issues = p.f.store.native
				p.f.restart(t)
			}
			first, second := importRecord(42), importRecord(100)
			q := importArgs(t, first, second)
			checkImportDryAndApply(t, p, q)
			checkImportedRead(t, p, first)
			checkImportedRead(t, p, second)
			checkImportReplay(t, p)
			checkImportConflicts(t, p)
			checkImportAfterEdit(t, p, q)
		})
	}
}
func checkImportDryAndApply(t *testing.T, p *peopleFixture, q apicontract.IssueImportArgs) {
	t.Helper()
	q.DryRun = true
	result, err := importCall(t, p, q)
	if err != nil || result.Count != 2 || result.HistoryEntries != 4 || !result.DryRun || result.Outcome != "No issues imported." {
		t.Fatal(result, err)
	}
	if _, err = p.f.s.PrepareIssueGet(t.Context(), adminCaller(), apicontract.IssueGetArgs{ID: 42}); err == nil {
		t.Fatal("dry run wrote issue")
	}
	q.DryRun = false
	result, err = importCall(t, p, q)
	if err != nil || result.AlreadyImported {
		t.Fatal(result, err)
	}
}
func checkImportReplay(t *testing.T, p *peopleFixture) {
	t.Helper()
	result, err := importCall(t, p, importArgs(t, importRecord(100), importRecord(42)))
	if err != nil || !result.AlreadyImported {
		t.Fatal("record order changed replay", result, err)
	}
	if _, err = importCall(t, p, importArgs(t, importRecord(42))); err == nil {
		t.Fatal("subset recognized as complete replay")
	}
	created := createTask(t, p, "origin", "new")
	if created.ID != 101 {
		t.Fatal("ID continuation lost", created.ID)
	}
	if _, err = importCall(t, p, importArgs(t, importRecord(created.ID))); err == nil {
		t.Fatal("ordinary issue overwritten")
	}
}
func checkImportConflicts(t *testing.T, p *peopleFixture) {
	t.Helper()
	changed := importRecord(42)
	changed.Title = "different record set"
	if _, err := importCall(t, p, importArgs(t, changed, importRecord(100))); err == nil {
		t.Fatal("changed record set overwrote IDs")
	}
	if _, err := importCall(t, p, importArgs(t, importRecord(7), importRecord(100))); err == nil {
		t.Fatal("mixed file accepted")
	}
	if _, err := p.f.s.PrepareIssueGet(t.Context(), adminCaller(), apicontract.IssueGetArgs{ID: 7}); err == nil {
		t.Fatal("partial import")
	}
}

func checkImportedRead(t *testing.T, p *peopleFixture, want model.IssueImportRecord) {
	t.Helper()
	call, err := p.f.s.PrepareIssueGet(t.Context(), adminCaller(), apicontract.IssueGetArgs{ID: want.ID})
	got, err := execute(t.Context(), call, err)
	if err != nil {
		t.Fatal(err)
	}
	source := got.Imported
	if source == nil || got.Lock != nil || got.Author != want.Author || got.Closer != want.Closer {
		t.Fatal("provenance or plain identities lost", got)
	}
	assertImportedProjection(t, got, want)

	// Returned slices must not mutate the stored imported history.
	got.History[0].Text = "changed outside store"
	again, err := p.f.c.People.Issues.ScopedIssue(t.Context(), stateport.IssueScope{Global: true}, want.ID)
	if err != nil || reflect.DeepEqual(got.History, again.History) {
		t.Fatal("history alias", err)
	}
}
func assertImportedProjection(t *testing.T, got model.Issue, want model.IssueImportRecord) {
	t.Helper()
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	var projection model.IssueImportRecord
	if err = json.Unmarshal(raw, &projection); err != nil {
		t.Fatal(err)
	}
	actual, err := json.Marshal(projection)
	if err != nil {
		t.Fatal(err)
	}
	expected, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	if string(actual) != string(expected) {
		t.Fatal("imported fields/timestamps/history changed", string(actual), string(expected))
	}

}

func checkImportAfterEdit(t *testing.T, p *peopleFixture, q apicontract.IssueImportArgs) {
	t.Helper()
	call, err := p.f.s.PrepareIssueUpdate(t.Context(), adminCaller(), apicontract.IssueUpdateArgs{ID: 42, Title: new("local edit")})
	if _, err = execute(t.Context(), call, err); err != nil {
		t.Fatal(err)
	}
	r, err := importCall(t, p, q)
	if err != nil || !r.AlreadyImported {
		t.Fatal("local edit destroyed replay provenance", r, err)
	}
	got, err := p.f.c.People.Issues.ScopedIssue(t.Context(), stateport.IssueScope{Global: true}, 42)
	if err != nil || got.Title != "local edit" || len(got.History) != 2 {
		t.Fatal("replay replaced local edit/history", err)
	}
}

func TestIssueImportRequiresLiveUnscopedAdministrator(t *testing.T) {
	p := people(t)
	q := importArgs(t, importRecord(9))
	for _, caller := range []apicontract.Caller{workerCaller(), {}} {
		_, err := p.f.s.PrepareIssueImport(t.Context(), caller, q)
		if err == nil {
			t.Fatal("non-administrator prepared import", caller)
		}
	}
	call, err := p.f.s.PrepareIssueImport(t.Context(), adminCaller(), q)
	if err != nil {
		t.Fatal(err)
	}
	revoked := admin()
	revoked.Admin = false
	p.f.store.Actors[revoked.Fingerprint] = revoked
	_, err = call(t.Context())
	if err == nil {
		t.Fatal("administrator revocation after prepare was ignored")
	}
	requireCode(t, err, publicerr.Forbidden)
}

func TestIssueImportRefusesScopedAdministratorAndMalformedInput(t *testing.T) {
	p := people(t)
	a := admin()
	a.Scope = []string{"origin"}
	p.f.store.Actors[a.Fingerprint] = a
	if _, err := p.f.s.PrepareIssueImport(t.Context(), adminCaller(), importArgs(t, importRecord(9))); err == nil {
		t.Fatal("scoped admin accepted")
	}
	p.f.store.Actors[a.Fingerprint] = admin()
	for _, data := range []string{"{}", "null", strings.Repeat("x", apicontract.RequestBytes+1), strings.Repeat(" ", apicontract.AdminBytes) + "{}"} {
		if _, err := p.f.s.PrepareIssueImport(t.Context(), adminCaller(), apicontract.IssueImportArgs{JSONL: data}); err == nil {
			t.Fatal("invalid or unbounded import prepared")
		}
	}
}

func countedJSONL(data string) string {
	count := strings.Count(strings.TrimSuffix(data, "\n"), "\n") + 1
	return fmt.Sprintf("{\"count\":%d}\n", count) + data
}
