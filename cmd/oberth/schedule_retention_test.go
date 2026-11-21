//go:build linux

// These tests exercise the Linux server and Git receive path.

package main

import (
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/gitcache/gittest"
	"github.com/oberthci/oberth/internal/nativebinding"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stateport"
)

func TestCompositionPublishesAndCleansScheduleAdmissions(t *testing.T) {
	t.Parallel()
	f := composeFixture(t, composeBinary(t))
	commit := f.prepareScheduledObservation(t)
	cancel, done := f.run(t)
	defer cancel()
	f.checkStarting(t, done)
	f.waitStarted(t, done)
	parent := f.scheduledParent(t, commit)
	digests := []string{parent.Spec.WorkflowMetadata.Annotations[nativebinding.AdmissionAnnotation]}
	for i := 1; i < 3; i++ {
		parent = f.scheduledParent(t, f.updateScheduledWorkflow(t, i))
		digests = append(digests, parent.Spec.WorkflowMetadata.Annotations[nativebinding.AdmissionAnnotation])
	}
	repo, err := f.owners.c.store.Repository(t.Context(), gittest.Repository, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.owners.c.store.ScheduleAdmission(t.Context(), repo.ID, "nightly", digests[0])
	if !errors.As(err, new(stateport.ScheduleAdmissionExpiredError)) {
		t.Fatal("publication retained a superseded admission", err)
	}
	for _, digest := range digests[1:] {
		if _, err = f.owners.c.store.ScheduleAdmission(t.Context(), repo.ID, "nightly", digest); err != nil {
			t.Fatal("publication lost current/native predecessor", err)
		}
	}
	view, err := f.owners.c.store.ScheduleAdmission(t.Context(), repo.ID, "nightly", digests[2])
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := admissioncontract.Seal(view)
	if err != nil {
		t.Fatal(err)
	}
	f.checkScheduleCleanupPorts(t, prepared, repo.ID)
}

func (f *composedFixture) updateScheduledWorkflow(t *testing.T, index int) string {
	t.Helper()
	document := fmt.Sprintf("apiVersion: argoproj.io/v1alpha1\nkind: Workflow\nmetadata: {generateName: build-}\nspec:\n  entrypoint: build\n  templates:\n  - name: build\n    container: {image: 'oberth.ci/build:1', command: [echo], args: ['revision %d']}\n", index)
	f.git.Write(t, ".oberth/build.yaml", []byte(document))
	commit := f.git.CommitFile(t, "schedule revision")
	f.git.Git(t, f.git.Work, "push", f.git.Bare, "HEAD:refs/heads/main")
	gittest.ExpireRefresh(t, filepath.Join(filepath.Dir(f.config.DatabaseFile), "git", "origin", "team", "widget.git"))
	if err := f.owners.c.schedules.SyncAll(t.Context()); err != nil {
		t.Fatal(err)
	}
	return commit
}

// Invoke the configured cleanup ports against real retained rows, so a nonnil
// no-op binding cannot satisfy the composition contract.
func (f *composedFixture) checkScheduleCleanupPorts(t *testing.T, p *admissioncontract.PreparedAdmission, id int64) {
	t.Helper()
	cfg := f.owners.c.scheduleConfig
	view := p.View()
	if err := cfg.ForgetAdmission(t.Context(), id, view.ScheduleEntry); err != nil {
		t.Fatal(err)
	}
	_, err := f.owners.c.store.ScheduleAdmission(t.Context(), id, view.ScheduleEntry, view.Digest)
	if publicerr.Classify(err).Code() != publicerr.NotFound {
		t.Fatal("forget port left retained snapshots", err)
	}
	if err = cfg.RetainAdmission(t.Context(), p); err != nil {
		t.Fatal(err)
	}
	if _, err = f.owners.c.store.ScheduleAdmission(t.Context(), id, view.ScheduleEntry, view.Digest); err != nil {
		t.Fatal("retain port did not restore snapshot", err)
	}
	if err = cfg.PruneAdmissions(t.Context(), id, nil); err != nil {
		t.Fatal(err)
	}
	_, err = f.owners.c.store.ScheduleAdmission(t.Context(), id, view.ScheduleEntry, view.Digest)
	if publicerr.Classify(err).Code() != publicerr.NotFound {
		t.Fatal("prune port left unused entry", err)
	}
}
