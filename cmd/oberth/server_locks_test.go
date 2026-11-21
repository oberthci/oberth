package main

import (
	"slices"
	"testing"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/nativebinding"
	"github.com/oberthci/oberth/internal/service"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Only a stale holder carries the command; Workflows of unregistered
// repositories are left out; the command restarts the installed controller.
func TestRunLocksNameRunsAndClearOnlyStaleHolders(t *testing.T) {
	t.Parallel()
	run := func(repo, id string) *wf.Workflow {
		return &wf.Workflow{ObjectMeta: meta.ObjectMeta{Labels: map[string]string{nativebinding.RepositoryLabel: repo}, Annotations: map[string]string{nativebinding.RunIDAnnotation: id}}}
	}
	const lock = "runs/ConfigMap/oberth-sync-limits/heavy-go"
	facts := []argojob.LockFact{{Lock: lock, Role: argojob.LockHolder, Workflow: run("1", "live")}, {Lock: lock, Role: argojob.LockWaiter, Workflow: run("1", "queued")}, {Lock: lock, Role: argojob.LockStale, Workflow: run("1", "ended")}, {Lock: lock, Role: argojob.LockStale, Workflow: run("2", "foreign")}}
	clear := lockClearCommand("argo-system")
	if clear != "kubectl -n argo-system rollout restart deployment/oberth-controller-workflow-controller" {
		t.Fatal("clear command", clear)
	}
	got := runLocks(facts, map[string]string{"1": "origin/team/widget"}, clear)
	want := []service.Lock{{Repo: "origin/team/widget", RunID: "live", Name: lock}, {Repo: "origin/team/widget", RunID: "queued", Name: lock, Waiting: true}, {Repo: "origin/team/widget", RunID: "ended", Name: lock, Clear: clear}}
	if !slices.Equal(got, want) {
		t.Fatalf("run locks\n got %+v\nwant %+v", got, want)
	}
}
