package argojob_test

import (
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/app"
	"github.com/oberthci/oberth/internal/gitcache/gittest"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/sourcecontract"
	"github.com/oberthci/oberth/internal/stateport"
)

func TestScheduleRefusesGitValidTemplateBranch(t *testing.T) {
	t.Parallel()
	git := gittest.New(t)
	git.Write(t, ".oberth/build.yaml", []byte(scheduleWorkflow))
	commit := git.CommitFile(t, "template branch workflow")
	ref := "refs/heads/{{workflow.name}}"
	git.Git(t, git.Work, "check-ref-format", ref)
	git.Git(t, git.Work, "push", git.Bare, "HEAD:"+ref)
	cache := git.Cache(t, nil)
	s := compositionStore(t)
	registerCompositionRepository(t, s, gittest.Repository)
	preparer := scheduleSubmitter(t, cache, s)
	request := app.Request{RepositoryID: 1, Repository: gittest.Repository, Ref: sourcecontract.Ref{Name: ref, ObjectSHA: commit, CommitSHA: commit}, Trigger: model.TriggerBranch}
	request.Actor.Identity, request.Actor.AuthMethod = "fixture", "system"
	inspected, err := preparer.Check(t.Context(), request)
	compositionOK(t, err)
	if inspected.View().Ref != ref {
		t.Fatal("branch check changed literal ref")
	}
	request.ScheduleEntry = "nightly"
	_, err = preparer.PrepareSchedule(t.Context(), request)
	if err == nil || !publicerr.Permanent(err) || !strings.Contains(err.Error(), "Rename the branch") {
		t.Fatal("schedule accepted a templated ref", err)
	}
	resolved, err := cache.ResolveRef(t.Context(), gittest.Repository, ref)
	compositionOK(t, err)
	if resolved.Name != ref || resolved.CommitSHA != commit {
		t.Fatal("Git resolution changed literal ref", resolved)
	}
	runs, err := s.Runs(t.Context(), stateport.RunFilter{Limit: 10})
	compositionOK(t, err)
	if len(runs) != 0 {
		t.Fatal("schedule refusal produced a run")
	}
}
