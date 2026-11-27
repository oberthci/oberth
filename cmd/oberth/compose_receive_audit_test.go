//go:build linux

package main

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/gitcache/gittest"
	"github.com/oberthci/oberth/internal/identityname"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/store"
	"github.com/oberthci/oberth/internal/workflowpolicy"
	"github.com/oberthci/oberth/pkg/argoworkflow"
)

func TestReceiveRefusalAuditMessageBounds(t *testing.T) {
	if len(codeReceiveRefused) > 40 {
		t.Fatal("receive refusal code exceeds its bound")
	}
	if err := publicerr.CheckMessage(reasonRefusalAudit, 80); err != nil {
		t.Fatal(err)
	}
	if err := publicerr.CheckMessage(remedyRefusalAudit, 120); err != nil {
		t.Fatal(err)
	}
}

func TestComposedReceiveRefusalsAreAuditedOnce(t *testing.T) {
	f := composeFixture(t, composeBinary(t))
	initial := f.prepareCommit(t)
	f.prepareAdministrator(t)
	document := strings.Replace(composedWorkflow, "  - name: build\n", "  - name: build\n    metadata:\n      annotations:\n        "+argoworkflow.SecretPathsAnnotation+": registry/publish\n", 1)
	f.git.Write(t, ".oberth/wrong-step.yaml", []byte(document+"# private-fixture-marker\n"))
	f.git.Write(t, ".oberth/build.yaml", []byte(strings.ReplaceAll(document, "build", "release-publish-images")))
	commit := f.git.CommitFile(t, "refusal fixtures")
	cancel, done := f.run(t)
	defer cancel()
	f.checkStarting(t, done)
	f.waitStarted(t, done)
	f.bindClient(t)
	grant := workflowpolicy.GrantKey{Key: identityname.Key{Repository: gittest.Repository, File: ".oberth/wrong-step.yaml", Step: "publish"}, Path: "registry/publish"}
	if err := f.owners.c.store.SecretGrant(t.Context(), stateport.Action{Actor: "administrator"}, grant, false); err != nil {
		t.Fatal(err)
	}
	grant.File, grant.Step = ".oberth/release.yaml", "release-publish-images"
	if err := f.owners.c.store.SecretGrant(t.Context(), stateport.Action{Actor: "administrator"}, grant, false); err != nil {
		t.Fatal(err)
	}
	for i, file := range []string{".oberth/build.yaml", ".oberth/wrong-step.yaml"} {
		f.assertRecordedReceiveRefusal(t, file, commit, i+1)
	}
	f.git.Write(t, ".oberth/build.yaml", []byte(composedWorkflow))
	f.git.CommitFile(t, "ordinary branch workflow")
	if output, err := f.pushResult(t, "refs/heads/accepted", "-o", "oberth.run=1"); err != nil {
		t.Fatal("ordinary accepted push changed", output, err)
	}
	f.waitMatchingRun(t, func(r stateport.Run) bool { return r.Ref == "refs/heads/accepted" && r.Origin.WorkflowUID != "" })
	rows, err := f.owners.c.store.AdministrativeAudit(t.Context(), nil, stateport.AdministrativeFilter{Action: "receive.refused", Limit: 20})
	if err != nil || len(rows) != 2 {
		t.Fatal("accepted push added a refusal record", rows, err)
	}
	f.assertRefusalAuditAPI(t)
	f.assertNativeRefusalAudit(t, initial)
	f.assertOpaqueRefusalAudit(t)
}

func (f *composedFixture) assertOpaqueRefusalAudit(t *testing.T) {
	t.Helper()
	event := gitcache.ReceiveEvent{Actor: "agent", Repository: gittest.Repository}
	if err := f.owners.refusedReceive(t.Context(), event, errors.New("private-fixture-marker")); err != nil {
		t.Fatal(err)
	}
	rows, err := f.owners.c.store.AdministrativeAudit(t.Context(), nil, stateport.AdministrativeFilter{Action: "receive.refused", Limit: 20})
	if err != nil || len(rows) != 4 {
		t.Fatal("opaque refusal did not leave one record", rows, err)
	}
	var detail store.ReceiveRefusal
	if err := json.Unmarshal([]byte(rows[0].Details), &detail); err != nil {
		t.Fatal(err)
	}
	if detail.Code != "receive_refused" {
		t.Fatal("opaque refusal lost its bounded reason code", detail)
	}
	checkReceiveRefusalPrivacy(t, rows[0].Details)
}

func (f *composedFixture) assertNativeRefusalAudit(t *testing.T, initial string) {
	t.Helper()
	const ref = "refs/heads/main"
	if output, err := f.pushResult(t, ref, "-o", "oberth.run=1"); err == nil {
		t.Fatal("agent default push accepted", output)
	}
	rows, err := f.owners.c.store.AdministrativeAudit(t.Context(), nil, stateport.AdministrativeFilter{Action: "receive.refused", Limit: 20})
	if err != nil || len(rows) != 3 {
		t.Fatal("native hook refusal did not leave one record", rows, err)
	}
	var detail store.ReceiveRefusal
	if err := json.Unmarshal([]byte(rows[0].Details), &detail); err != nil {
		t.Fatal(err)
	}
	want := []store.ReceiveRefusalRef{{Ref: ref, Old: initial, New: f.git.Git(t, f.git.Work, "rev-parse", "HEAD")}}
	if detail.Code != string(publicerr.InvalidInput) || !reflect.DeepEqual(detail.Updates, want) {
		t.Fatal("native hook refusal lost its reason code or updates", detail)
	}
	if _, _, err := f.owners.c.store.Audit(t.Context(), 0, 200); err != nil {
		t.Fatal("native refusal broke the chain", err)
	}
}

func TestComposedShutdownFlushesReceiveRefusals(t *testing.T) {
	f := composeFixture(t, composeBinary(t))
	f.prepareCommit(t)
	cancel, done := f.run(t)
	defer cancel()
	f.checkStarting(t, done)
	f.waitStarted(t, done)
	for range 20 {
		if err := f.owners.c.store.RecordReceiveRefusal(t.Context(), "agent", gittest.Repository, store.ReceiveRefusal{Code: "grant"}); err != nil {
			t.Fatal(err)
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal("server shutdown failed", err)
	}
	s, err := store.Open(t.Context(), f.config.DatabaseFile, f.storeOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	}()
	rows, err := s.AdministrativeAudit(t.Context(), nil, stateport.AdministrativeFilter{Action: "receive.refused.more", Limit: 10})
	if err != nil || len(rows) != 1 || rows[0].Details != `{"more_refusals":12}` {
		t.Fatal("server shutdown lost coalesced refusal count", rows, err)
	}
}

func (f *composedFixture) assertRefusalAuditAPI(t *testing.T) {
	t.Helper()
	c := f.administratorClient(t)
	var page apicontract.AuditPage
	if err := c.Invoke(t.Context(), "audit_list", apicontract.AuditListArgs{Action: "receive.refused", Repo: gittest.Repository}, &page); err != nil || len(page.Records) != 2 {
		t.Fatal("administrator audit_list hid receive refusals", page, err)
	}
	_, _, err := f.owners.c.store.Audit(t.Context(), 0, 200)
	if err != nil {
		t.Fatal("refusals broke the audit chain", err)
	}
}

func (f *composedFixture) assertRecordedReceiveRefusal(t *testing.T, file, commit string, count int) {
	t.Helper()
	const ref = "refs/heads/probe"
	if output, err := f.pushResult(t, ref, "-o", "oberth.run=1", "-o", "oberth.file="+file); err == nil {
		t.Fatal("probe accepted", output)
	}
	rows, err := f.owners.c.store.AdministrativeAudit(t.Context(), nil, stateport.AdministrativeFilter{Action: "receive.refused", Limit: 20})
	if err != nil || len(rows) != count {
		t.Fatal("refusal did not leave exactly one audit record", rows, err)
	}
	row := rows[0]
	if row.Actor != f.joinedActor(t).Identity || row.ResourceID != gittest.Repository {
		t.Fatal("refusal attribution differs", row)
	}
	var detail store.ReceiveRefusal
	if err := json.Unmarshal([]byte(row.Details), &detail); err != nil {
		t.Fatal(err)
	}
	want := store.ReceiveRefusal{Code: "grant", File: file, Step: "release-publish-images", Updates: []store.ReceiveRefusalRef{{Ref: ref, New: commit}}}
	if file == ".oberth/wrong-step.yaml" {
		want.Step = "build"
	}
	if !reflect.DeepEqual(detail, want) {
		t.Fatal("refusal metadata differs", detail, want)
	}
	checkReceiveRefusalPrivacy(t, row.Details)
}

func checkReceiveRefusalPrivacy(t *testing.T, details string) {
	t.Helper()
	for _, private := range []string{"private-fixture-marker", "registry/publish", "remedy"} {
		if strings.Contains(details, private) {
			t.Fatal("refusal audit retained authored bytes or full diagnostics", details)
		}
	}
}
