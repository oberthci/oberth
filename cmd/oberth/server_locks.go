package main

import (
	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/installer"
	"github.com/oberthci/oberth/internal/nativebinding"
	"github.com/oberthci/oberth/internal/service"
)

// runLocks names each native lock fact by run and repository; facts of
// Workflows outside the registered repositories are left out. A stale holder
// carries clear, the command that releases it.
func runLocks(facts []argojob.LockFact, repos map[string]string, clear string) []service.Lock {
	out := []service.Lock{}
	for _, f := range facts {
		repo := repos[f.Workflow.Labels[nativebinding.RepositoryLabel]]
		if repo == "" {
			continue
		}
		l := service.Lock{Repo: repo, RunID: f.Workflow.Annotations[nativebinding.RunIDAnnotation], Name: f.Lock, Permits: f.Permits, Steps: f.Steps, Waiting: f.Role == argojob.LockWaiter}
		if f.Role == argojob.LockStale {
			l.Clear = clear
		}
		out = append(out, l)
	}
	return out
}

// lockClearCommand is the command that rebuilds the workflow controller's
// lock count from Workflow statuses, where an ended run holds nothing.
func lockClearCommand(namespace string) string {
	return "kubectl -n " + namespace + " rollout restart deployment/" + installer.ControllerDeployment
}
