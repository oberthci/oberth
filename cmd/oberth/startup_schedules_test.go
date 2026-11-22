package main

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/oberthci/oberth/internal/cronsync"
	cronfake "github.com/oberthci/oberth/internal/cronsync/fake"
	"github.com/oberthci/oberth/internal/gitport"
	"github.com/oberthci/oberth/internal/nativebinding"
)

// startupRepositories is a catalogue of three repositories told apart by
// their default branches: one whose schedule read hangs until released, one
// whose schedule Git is unavailable and a healthy one.
type startupRepositories struct {
	hang, fail, healthy cronsync.Repository
	release, pruned     chan struct{}
	once                sync.Once
}

func (s *startupRepositories) all() []cronsync.Repository {
	return []cronsync.Repository{s.hang, s.fail, s.healthy}
}

func (s *startupRepositories) git() *cronfake.Git {
	git := cronfake.NewGit(func(ctx context.Context, verb, name string) error {
		switch {
		case verb == "resolve" && name == "refs/heads/"+s.hang.DefaultBranch:
			select {
			case <-s.release:
			case <-ctx.Done():
			}
			return ctx.Err()
		case verb == "resolve" && name == "refs/heads/"+s.fail.DefaultBranch:
			return &gitport.UnavailableError{Op: "forge"}
		}
		return nil
	})
	// The healthy repository's default branch no longer has a schedule file.
	git.PutRef(s.healthy.Qualified, gitport.Ref{Name: "refs/heads/" + s.healthy.DefaultBranch, ObjectSHA: strings.Repeat("a", 40), CommitSHA: strings.Repeat("a", 40)})
	return git
}

// parent is an owned CronWorkflow left from an earlier admission.
func parent(t *testing.T, namespace string, repo cronsync.Repository) *wfv1.CronWorkflow {
	t.Helper()
	name, err := nativebinding.CronName(repo.ID, "nightly")
	if err != nil {
		t.Fatal(err)
	}
	object := &wfv1.CronWorkflow{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: map[string]string{nativebinding.ManagedLabel: nativebinding.ManagedValue}}}
	object.Spec.WorkflowSpec.Arguments.Parameters = []wfv1.Parameter{{Name: "oberth-schedule", Value: wfv1.AnyStringPtr("nightly")}}
	return object
}

func (s *startupRepositories) lookup(_ context.Context, key string) (cronsync.Repository, bool, error) {
	for _, repo := range s.all() {
		if repo.Qualified == key {
			return repo, true, nil
		}
	}
	return cronsync.Repository{}, false, nil
}

// wire gives the fixture's schedules this catalogue, its Git and a native
// store holding the failing and the healthy repository's earlier parents.
func (s *startupRepositories) wire(t *testing.T, f *serverFixture) {
	healthyName, err := nativebinding.CronName(s.healthy.ID, "nightly")
	if err != nil {
		t.Fatal(err)
	}
	f.deps.constructors.WiringNew = func(ctx context.Context, c *composition) (Part, error) {
		p, err := f.wiring(ctx, c)
		native := cronfake.NewNative(c.config.PipelineNamespace, c.clock.Now, func(_ context.Context, verb, name string) error {
			if verb == "delete" && name == healthyName {
				close(s.pruned)
			}
			return nil
		})
		for _, repo := range []cronsync.Repository{s.fail, s.healthy} {
			if _, err := native.Create(ctx, parent(t, c.config.PipelineNamespace, repo), metav1.CreateOptions{}); err != nil {
				return p, err
			}
		}
		cfg := &c.scheduleConfig
		cfg.Client, cfg.Git, cfg.RepositoryAttempt = native, s.git(), time.Minute
		cfg.Repositories = func(context.Context) ([]cronsync.Repository, error) { return s.all(), nil }
		cfg.Repository = s.lookup
		return p, err
	}
}

// A startup pass that cannot read the catalogue reached no repository, and
// no later event repeats it: the part fails and the server stops, so the
// restart retries it.
func TestStartupPassThatCannotReadTheCatalogueStopsTheServer(t *testing.T) {
	f := fixtureServer(t)
	f.deps.constructors.WiringNew = func(ctx context.Context, c *composition) (Part, error) {
		p, err := f.wiring(ctx, c)
		c.scheduleConfig.Repositories = func(context.Context) ([]cronsync.Repository, error) {
			return nil, errors.New("catalogue unavailable")
		}
		return p, err
	}
	// serve directly: the server stops right after it reports ready, which
	// the fixture's own start would take for a readiness failure.
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- serve(ctx, f.config, f.deps, f.trace) }()
	select {
	case err := <-done:
		if !strings.Contains(f.trace.String(), "ready:") {
			t.Fatal("the server stopped before it reported ready", err, f.trace.String())
		}
		if err == nil || !strings.Contains(err.Error()+compositionRefusalDetail(err), "schedules") {
			t.Fatal("the server stopped without naming the failed part", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("an unstarted startup pass left the server running", f.trace.String())
	}
}

// The startup schedule pass runs after the server reports ready: a
// repository whose schedule read hangs cannot hold readiness, the healthy one
// is reconciled afterwards (the parent of its removed schedule file is
// pruned) and the failing one shows its state.
func TestServerIsReadyBeforeItsStartupSchedulePass(t *testing.T) {
	f := fixtureServer(t)
	repos := &startupRepositories{
		hang:    cronsync.Repository{ID: 1, Qualified: "origin/team/hang", DefaultBranch: "hang"},
		fail:    cronsync.Repository{ID: 2, Qualified: "origin/team/fail", DefaultBranch: "fail"},
		healthy: cronsync.Repository{ID: 3, Qualified: "origin/team/healthy", DefaultBranch: "main"},
		release: make(chan struct{}), pruned: make(chan struct{}),
	}
	t.Cleanup(func() { repos.once.Do(func() { close(repos.release) }) })
	repos.wire(t, f)
	done := f.start(t.Context())
	if !strings.Contains(f.c.health.metrics(t.Context()), "oberth_schedules_synced 0\n") {
		t.Fatal("the metric reported a synced pass while a repository's read still hangs")
	}
	repos.once.Do(func() { close(repos.release) })
	select {
	case <-repos.pruned:
	case <-time.After(10 * time.Second):
		t.Fatal("the healthy repository was not reconciled after the server was ready", f.trace.String())
	}
	status, err := f.c.schedules.Status(t.Context(), repos.fail, "nightly")
	if err != nil || status.Error != "schedule Git is unavailable" {
		t.Fatal("the failing repository does not show its state", status.Error, err)
	}
	// The pass has ended (Status waited for its lock) with the failing
	// repository's error; the server keeps serving.
	select {
	case err := <-done:
		t.Fatal("a schedule failure stopped the server", err)
	default:
	}
	if err := f.c.health.ready(t.Context()); err != nil {
		t.Fatal("a schedule failure made the server unready", err)
	}
}
