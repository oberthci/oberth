package app

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/runlog"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/store"
	"github.com/oberthci/oberth/internal/storetest"
	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/pipeline"
)

// Only producer transport/refs/blobs use the real Git cache. Candidate policy,
// Kubernetes observations and publication effects retain the existing fixture.
type directProducerPolicy struct {
	*directPolicyFixture
	cache *gitcache.Cache
	reads int
}

func (p *directProducerPolicy) ReadDefaultBlob(ctx context.Context, repo, file string, limit int) (string, []byte, bool, error) {
	if repo == "github/org/shared" {
		p.reads++
		return p.cache.ReadDefaultBlob(ctx, repo, file, limit)
	}
	return p.directPolicyFixture.ReadDefaultBlob(ctx, repo, file, limit)
}

func directRealTagFixture(t *testing.T) (*directPlacementFixture, *directProducerPolicy, func(string)) {
	t.Helper()
	ctx := t.Context()
	work, remote := t.TempDir(), filepath.Join(t.TempDir(), "upstream.git")
	git := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.invalid", "GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.invalid")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	source, body := directFragmentSources()
	git(work, "init", "-b", "main")
	writePipeline(t, work, "fragment.yaml", string(body))
	git(work, "add", ".")
	git(work, "commit", "-m", "published fragment")
	original := git(work, "rev-parse", "HEAD")
	git(work, "tag", "-a", "v1", "-m", "published original")
	git(work, "init", "--bare", "--initial-branch=main", remote)
	git(work, "push", remote, "main", "refs/tags/v1")
	cache, err := gitcache.New(gitcache.Config{AllowLocalUpstreams: true, Root: t.TempDir(),
		Upstream: func(string) (string, error) { return remote, nil },
		RepoQualifier: func(string) (gitcache.RepoQualification, error) {
			return gitcache.RepoQualification{UpstreamName: "github", Org: "org"}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	cached, err := cache.Ensure(ctx, "github/org/shared")
	if err != nil {
		t.Fatal(err)
	}
	key, err := argoworkflow.ParseFragmentRef("github/org/shared@v1")
	if err != nil {
		t.Fatal(err)
	}
	loader, err := NewGitFragmentLoader(cache, fakeRegistry{known: map[string]bool{key.Repo: true}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	fragment, err := loader.Load(ctx, key, pipeline.TriggerCI, model.RefBranch, "branch")
	if err != nil || fragment.SHA != original {
		t.Fatal("initial production loader", fragment.SHA, err)
	}
	f := newDirectPlacementFixtureInputs(t, 0, source, map[argoworkflow.FragmentKey]argoworkflow.Fragment{key: fragment})
	f.jobs.fragments = loader
	policy := &directProducerPolicy{directPolicyFixture: f.jobs.resourceBlobs.(*directPolicyFixture), cache: cache}
	f.jobs.SetResourceBlobs(policy)
	mutate := func(mode string) {
		t.Helper()
		switch mode {
		case "unchanged":
		case "deleted":
			git(work, "push", remote, ":refs/tags/v1")
		case "moved":
			// Same fragment bytes: only current upstream tag/commit authority changes.
			git(work, "commit", "--allow-empty", "-m", "new published commit")
			git(work, "tag", "-f", "-a", "v1", "-m", "moved release")
			git(work, "push", remote, "main", "+refs/tags/v1:refs/tags/v1")
		case "offline":
			if err := os.Rename(remote, remote+".offline"); err != nil {
				t.Fatal(err)
			}
		default:
			t.Fatal(mode)
		}
		if mode != "offline" {
			if _, _, _, err := cache.ReadDefaultBlob(gitcache.WithFreshPolicy(ctx), key.Repo, argoworkflow.FragmentFile, argoworkflow.MaxSourceBytes); err != nil {
				t.Fatal(err)
			}
			// The stale public tag still resolves and its commit remains on main.
			old, err := loader.Load(ctx, key, pipeline.TriggerCI, model.RefBranch, "branch")
			if err != nil || old.SHA != original {
				t.Fatal("ordinary public lookup changed", old.SHA, err)
			}
			public := git(cached.Path, "rev-parse", "refs/tags/v1^{commit}")
			if public != original {
				t.Fatal("authority refresh materialized public tags")
			}
			privateRef := "refs/oberth/upstream/tags/v1"
			switch mode {
			case "deleted":
				if got := git(cached.Path, "for-each-ref", "--format=%(refname)", privateRef); got != "" {
					t.Fatal("private tag not pruned", got)
				}
			case "moved":
				if got := git(cached.Path, "rev-parse", privateRef+"^{commit}"); got == original {
					t.Fatal("private tag not moved")
				}
			}
		}
	}
	return f, policy, mutate
}

func TestDirectProtectedFragmentRealUpstreamTagReceipt(t *testing.T) {
	for _, mode := range []string{"unchanged", "deleted", "moved", "offline"} {
		t.Run(mode, func(t *testing.T) {
			f, policy, mutate := directRealTagFixture(t)
			f.workflows.afterCreate = func() { mutate(mode) }
			err := f.executor.Execute(t.Context(), f.request, f.requirement, io.Discard)
			execution, readErr := f.database.KVMPreflight(t.Context(), f.request.Run.ID)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if mode == "unchanged" {
				if err != nil || execution.Receipt == nil {
					t.Fatal("unchanged upstream refused receipt", err)
				}
			} else if err == nil || execution.Receipt != nil {
				t.Fatal("changed upstream accepted receipt", err)
			}
			if !execution.Cleaned || f.workflows.posts != 1 || f.workflows.deletes != 1 {
				t.Fatal("original UID cleanup failed", execution, err)
			}
			if policy.reads == 0 {
				t.Fatal("producer was not refreshed")
			}
		})
	}
}

func TestDirectProtectedFragmentRealUpstreamTagPublication(t *testing.T) {
	for _, mode := range []string{"unchanged", "deleted", "moved", "offline"} {
		t.Run(mode, func(t *testing.T) {
			f, policy, mutate := directRealTagFixture(t)
			ctx := t.Context()
			if err := f.executor.Execute(ctx, f.request, f.requirement, io.Discard); err != nil {
				t.Fatal(err)
			}
			if _, err := f.database.SetRunWorkflowName(ctx, f.request.Run.ID, f.request.WorkflowName); err != nil {
				t.Fatal(err)
			}
			if err := f.jobs.CreateWorkflow(ctx, f.request); err != nil {
				t.Fatal(err)
			}
			mutate(mode)
			logs, err := runlog.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			scheduler, err := service.NewScheduler(service.SchedulerConfig{Store: f.database, Git: policy, Logs: logs, Jobs: f.jobs, ReleaseJobs: f.jobs, TrustedPreflight: f.executor, Auditor: f.database, WorkspaceRoot: t.TempDir(), MutationGate: func(context.Context) error { return nil }})
			if err != nil {
				t.Fatal(err)
			}
			pub, err := f.database.BeginPublication(ctx, model.PublicationSpec{RepoID: f.request.Repository.ID, RunID: f.request.Run.ID, RefKind: model.RefBranch, Ref: f.request.Run.Ref, ResultSHA: f.request.Run.SHA, Actor: f.request.Run.Actor})
			if err != nil {
				t.Fatal(err)
			}
			before := policy.reads
			result, err := scheduler.DeliverPublication(ctx, pub.ID)
			if policy.reads <= before {
				t.Fatal("publication did not refresh producer")
			}
			if mode == "unchanged" {
				if err != nil || result.Publication.Status != model.PublicationDelivered || policy.pushes != 1 {
					t.Fatal("unchanged upstream refused publication", err)
				}
			} else if policy.pushes != 0 || result.Publication.Status == model.PublicationDelivered {
				t.Fatal("changed upstream published", err)
			}
		})
	}
}

func TestDirectProtectedFragmentRealUpstreamTagRestartCleanup(t *testing.T) {
	for _, mode := range []string{"deleted", "moved", "offline"} {
		t.Run(mode, func(t *testing.T) {
			f, policy, mutate := directRealTagFixture(t)
			ctx := t.Context()
			f.workflows.lostReply = true
			if f.executor.Execute(ctx, f.request, f.requirement, io.Discard) == nil {
				t.Fatal("lost reply accepted")
			}
			mutate(mode)
			if err := f.database.Close(); err != nil {
				t.Fatal(err)
			}
			db, err := storetest.Open(ctx, f.databasePath, store.Options{})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = db.Close() }()
			f.workflows.readsUnavailable = false
			native, err := argojob.NewController(f.workflows, f.environment.kube, f.jobs.config)
			if err != nil {
				t.Fatal(err)
			}
			native.WithProtectedPlacementJournal(db, f.environment.definition.Transport).WithWorkflowCapabilityLoader(db.ActiveWorkflowCapability)
			jobs, err := NewArgoJobs(native, f.jobs.config, db, emptySecretAccess{}, nil)
			if err != nil {
				t.Fatal(err)
			}
			jobs.SetWorkflowCapabilityLoader(db.ActiveWorkflowCapability)
			jobs.SetResourceBlobs(policy)
			executor, err := NewGenericProtectedPreflights(jobs, db, f.executor.recipes, f.executor.workspaceRoot, func(context.Context) error { return nil })
			if err != nil {
				t.Fatal(err)
			}
			reads := policy.reads
			if _, err = executor.Recover(ctx); err != nil {
				t.Fatal(err)
			}
			execution, err := db.KVMPreflight(ctx, f.request.Run.ID)
			if err != nil || !execution.Cleaned || execution.Receipt != nil || f.workflows.posts != 1 || f.workflows.deletes != 1 {
				t.Fatal("original UID cleanup stranded or recreated", err)
			}
			if policy.reads != reads {
				t.Fatal("retained cleanup refreshed producer")
			}
			if _, err = db.VerifyAuditState(ctx); err != nil {
				t.Fatal(err)
			}
		})
	}
}
