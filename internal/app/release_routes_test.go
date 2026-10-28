package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/periapsis"
)

func TestReleaseRoutesUseOneDocumentForEveryRead(t *testing.T) {
	dir := t.TempDir()
	routed := strings.ReplaceAll(groupedPipeline("    oberth.ci/concurrency-group: deployment\n"), "name: main", "name: deploy")
	routed = strings.ReplaceAll(routed, "entrypoint: main", "entrypoint: root")
	routed = strings.ReplaceAll(routed, "  templates:\n", "  templates:\n    - name: root\n      dag:\n        tasks:\n        - name: deploy\n          template: deploy\n")
	writePipeline(t, dir, "deploy.yaml", routed)
	writePipeline(t, dir, "build.yaml", strings.ReplaceAll(groupedPipeline(""), "oberth.ci/size: M", "oberth.ci/size: S"))
	writePipeline(t, dir, "release.yaml", strings.ReplaceAll(groupedPipeline(""), "oberth.ci/size: M", "oberth.ci/size: XL"))
	jobs := &ArgoJobs{config: argojob.Config{ReleaseTagRoutes: argojob.ReleaseTagRoutes{"github/org/repo": {{Pattern: "deploy-*", File: ".oberth/deploy.yaml"}}}}}
	request := service.JobRequest{SourceDir: dir, UpstreamName: "github", UpstreamOrg: "org", Repository: model.Repository{Name: "repo"}, Run: model.Run{Release: true, RefKind: model.RefTag, Ref: "deploy-1"}}
	if size, err := jobs.PipelineSize(t.Context(), request); err != nil || size != periapsis.M {
		t.Fatalf("routed size=%s err=%v", size, err)
	}
	if group, err := jobs.PipelineConcurrencyGroup(t.Context(), request); err != nil || group != "deployment" {
		t.Fatalf("group=%q err=%v", group, err)
	}
	if plan, err := jobs.PlannedSteps(t.Context(), request); err != nil || len(plan) != 1 || plan[0].Step != "deploy" {
		t.Fatalf("plan=%+v err=%v", plan, err)
	}
	if resources, err := jobs.PipelineKubeVirt(t.Context(), request); err != nil || resources != (service.KubeVirtResources{}) {
		t.Fatalf("resources=%+v err=%v", resources, err)
	}
	if credentialed, err := jobs.PipelineCredentialed(t.Context(), request); err != nil || credentialed {
		t.Fatalf("credentialed=%v err=%v", credentialed, err)
	}
	request.Run.Ref = "v1"
	if size, err := jobs.PipelineSize(t.Context(), request); err != nil || size != periapsis.XL {
		t.Fatalf("default release changed: %s %v", size, err)
	}
	request.Run.Release = false
	request.Run.Trigger = "branch"
	request.Run.Ref = "deploy-1"
	request.Run.RefKind = model.RefBranch
	if size, err := jobs.PipelineSize(t.Context(), request); err != nil || size != periapsis.S {
		t.Fatalf("branch read release route: %s %v", size, err)
	}
	request.Run.Release = true
	request.Run.Ref = "deploy-1"
	request.Run.RefKind = model.RefTag
	request.Run.Trigger = "tag"
	if err := os.Remove(filepath.Join(dir, ".oberth", "deploy.yaml")); err != nil {
		t.Fatal(err)
	}
	if _, err := jobs.PipelineSize(t.Context(), request); err == nil || !strings.Contains(err.Error(), ".oberth/deploy.yaml") {
		t.Fatalf("missing route silently fell back: %v", err)
	}
}

func TestReleaseRoutesExecutionAndAuditBindSelectedFile(t *testing.T) {
	dir := t.TempDir()
	source := strings.ReplaceAll(groupedPipeline(""), "name: main", "name: deploy")
	source = strings.ReplaceAll(source, "entrypoint: main", "entrypoint: deploy")
	writePipeline(t, dir, "deploy.yaml", source)
	writePipeline(t, dir, "release.yaml", "this: is: invalid")
	control := &routeController{}
	var audit model.AuditActionSpec
	config := argojob.Config{Namespace: "pipelines", PipelineServiceAccount: "pipeline", CredentialedServiceAccount: "release", CISecretsServiceAccount: "ci", ExecutorServiceAccount: "executor", ReleaseTagRoutes: argojob.ReleaseTagRoutes{"github/org/repo": {{Pattern: "deploy-*", File: ".oberth/deploy.yaml"}}}}
	jobs, err := NewArgoJobs(control, config, &stubAuditor{onAppend: func(s model.AuditActionSpec) { audit = s }}, emptySecretAccess{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	config.ReleaseTagRoutes["github/org/repo"][0].File = ".oberth/release.yaml" // Startup snapshot must remain pinned.
	request := service.JobRequest{SourceDir: dir, JobName: "route-run", UpstreamName: "github", UpstreamOrg: "org", Repository: model.Repository{Name: "repo"}, Run: model.Run{ID: "route-run", Actor: "admin", Release: true, RefKind: model.RefTag, Ref: "deploy-1", SHA: strings.Repeat("a", 40)}}
	if err := jobs.CreateRelease(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	if string(control.request.Source) != source || control.request.RunTrigger != argoworkflow.RunTriggerTag {
		t.Fatal("execution read a different document")
	}
	var details map[string]any
	if err := json.Unmarshal([]byte(audit.Details), &details); err != nil {
		t.Fatal(err)
	}
	if details["pipeline_file"] != ".oberth/deploy.yaml" {
		t.Fatalf("audit lost route: %v", details)
	}
}

type routeController struct {
	argoControl
	request argojob.Request
}

func (c *routeController) Create(_ context.Context, r argojob.Request) (string, error) {
	c.request = r
	return r.Name, nil
}

func TestGrantFreeWIFReleaseAndBranchRefusalThroughEngine(t *testing.T) {
	for _, org := range []string{"org", ""} {
		t.Run("org="+org, func(t *testing.T) {
			key := "github/org/repo"
			if org == "" {
				key = "github/github/repo"
			}
			dir := t.TempDir()
			source := strings.Replace(groupedPipeline(""), "    - name: main\n", "    - name: main\n      metadata:\n        annotations:\n          oberth.ci/release-wif-role: deployer\n", 1)
			writePipeline(t, dir, "deploy.yaml", source)
			writePipeline(t, dir, "build.yaml", source)
			writePipeline(t, dir, "release.yaml", source)
			c := argojob.Config{Namespace: "pipelines", PipelineServiceAccount: "pipeline", CredentialedServiceAccount: "release", CISecretsServiceAccount: "ci", ExecutorServiceAccount: "executor", ReleaseTagRoutes: argojob.ReleaseTagRoutes{key: {{Pattern: "deploy-*", File: ".oberth/deploy.yaml"}}}, ReleaseWIF: &argojob.ReleaseWIFConfig{Namespace: "pipelines", Roles: map[string]argojob.ReleaseWIFRole{"deployer": {Provider: "projects/123/locations/global/workloadIdentityPools/deployers/providers/issuer", ServiceAccount: "deployer@example-project.iam.gserviceaccount.com"}}, Repositories: map[string]argojob.ReleaseWIFRepository{key: {Templates: map[string]string{"main": "deployer"}, StepServiceAccounts: map[string]string{"main": "deployment-step"}, StepRoutes: map[string]argojob.ReleaseTagRoute{"main": {Pattern: "deploy-*", File: ".oberth/deploy.yaml"}}}}}}
			control := &routeController{}
			jobs, err := NewArgoJobs(control, c, &stubAuditor{}, emptySecretAccess{}, nil)
			if err != nil {
				t.Fatal(err)
			}
			request := service.JobRequest{SourceDir: dir, JobName: "wif-route", UpstreamName: "github", UpstreamOrg: org, Repository: model.Repository{Name: "repo"}, Run: model.Run{ID: "wif-route", Actor: "admin", Release: true, RefKind: model.RefTag, Ref: "deploy-1", SHA: strings.Repeat("a", 40)}}
			if err := jobs.CreateRelease(t.Context(), request); err != nil {
				t.Fatal(err)
			}
			wf, err := argojob.Build(c, control.request)
			if err != nil {
				t.Fatal(err)
			}
			if wf.Spec.ServiceAccountName != "pipeline" || wf.Spec.Templates[0].ServiceAccountName != "deployment-step" {
				t.Fatalf("wrong grant-free identity: %+v", wf.Spec)
			}
			control.request = argojob.Request{}
			request.Run.Ref = "v1"
			request.Run.ID, request.JobName = "default-wif", "default-wif"
			if err := jobs.CreateRelease(t.Context(), request); err == nil || control.request.RunID != "" {
				t.Fatal("default v* release reused the demo route's identity")
			}
			control.request = argojob.Request{}
			request.JobName = "branch-route"
			request.Run.ID = "branch-route"
			request.Run.Release = false
			request.Run.Trigger = "branch"
			request.Run.Ref = "deploy-1"
			request.Run.RefKind = model.RefBranch
			if err := jobs.CreateCI(t.Context(), request); err == nil || control.request.RunID != "" {
				t.Fatal("branch reached controller with a release WIF identity")
			}
		})
	}
}

// Send a real annotated tag and Git pack through receive-pack, the durable
// outbox, ReceiveHandler and PushIngestor before reading the admitted run.
// This covers prefix stripping instead of calling AdmitRelease with a bare tag.
func TestReleaseRoutesForPushedTagsThroughReceiveAdapter(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	ctx := t.Context()
	work, bare := t.TempDir(), filepath.Join(t.TempDir(), "remote.git")
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
	git(work, "init", "-b", "main")
	writePipeline(t, work, "release.yaml", strings.ReplaceAll(groupedPipeline(""), "oberth.ci/size: M", "oberth.ci/size: XL"))
	writePipeline(t, work, "deploy.yaml", groupedPipeline(""))
	git(work, "add", ".")
	git(work, "commit", "-m", "release workflows")
	git(work, "init", "--bare", bare)
	git(work, "push", bare, "main")
	git(work, "--git-dir="+bare, "symbolic-ref", "HEAD", "refs/heads/main")
	cache, err := gitcache.New(gitcache.Config{Root: t.TempDir(),
		Upstream: func(string) (string, error) { return bare, nil },
		RepoQualifier: func(string) (gitcache.RepoQualification, error) {
			return gitcache.RepoQualification{UpstreamName: "github", Org: "org"}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "db.sqlite"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	upstream, err := db.CreateUpstream(ctx, model.UpstreamSpec{Name: "github", Kind: "github", BaseURL: "https://github.com/org"})
	if err != nil {
		t.Fatal(err)
	}
	repo, err := db.CreateRepository(ctx, model.RepositorySpec{Name: "repo", UpstreamID: upstream.ID, DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	logs, err := runlog.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	control := &routeController{}
	config := argojob.Config{Namespace: "pipelines", PipelineServiceAccount: "pipeline", CredentialedServiceAccount: "release", CISecretsServiceAccount: "ci", ExecutorServiceAccount: "executor", ReleaseTagRoutes: argojob.ReleaseTagRoutes{"github/org/repo": {{Pattern: "deploy-*", File: ".oberth/deploy.yaml"}}}}
	jobs, err := NewArgoJobs(control, config, &stubAuditor{}, emptySecretAccess{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	scheduler, err := service.NewScheduler(service.SchedulerConfig{Store: db, Git: unusedRouteGit{}, Logs: logs, Jobs: jobs, ReleaseJobs: jobs, Auditor: &stubAuditor{}, WorkspaceRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	pushes, err := service.NewPushIngestor(service.PushConfig{Repositories: db, Discoverer: registeredRouteDiscovery{}, Git: cache, Receives: db, Releases: scheduler})
	if err != nil {
		t.Fatal(err)
	}
	adapter, err := service.NewReceiveHandler(pushes)
	if err != nil {
		t.Fatal(err)
	}
	var received []gitcache.ReceiveEvent
	handler := gitcache.ReceiveHandlerFunc(func(ctx context.Context, event gitcache.ReceiveEvent) error {
		received = append(received, event)
		return adapter.HandleReceive(ctx, event)
	})
	for _, tc := range []struct {
		tag, file string
		size      periapsis.Size
	}{{"deploy-1", ".oberth/deploy.yaml", periapsis.M}, {"v1", ".oberth/release.yaml", periapsis.XL}} {
		git(work, "tag", "-a", tc.tag, "-m", tc.tag)
		object := git(work, "rev-parse", "refs/tags/"+tc.tag)
		commit := git(work, "rev-parse", "refs/tags/"+tc.tag+"^{commit}")
		// A normal receive-pack request: pkt-line ref update, flush and an
		// actual Git-generated pack. No fabricated ReceiveEvent or tag OID.
		packCmd := exec.CommandContext(ctx, "git", "-C", work, "pack-objects", "--stdout", "--revs")
		packCmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
		packCmd.Stdin = strings.NewReader(object + "\n")
		pack, err := packCmd.Output()
		if err != nil {
			t.Fatal(err)
		}
		update := strings.Repeat("0", 40) + " " + object + " refs/tags/" + tc.tag + "\x00report-status\n"
		var input, output, diagnostics bytes.Buffer
		_, _ = fmt.Fprintf(&input, "%04x%s0000", len(update)+4, update)
		_, _ = input.Write(pack)
		if err = cache.Receive(ctx, "github/org/repo", "admin", "", &input, &output, &diagnostics, handler); err != nil {
			t.Fatalf("receive %s: %v\n%s\n%s", tc.tag, err, output.Bytes(), diagnostics.Bytes())
		}
		if !strings.Contains(output.String(), "unpack ok") || !strings.Contains(output.String(), "ok refs/tags/"+tc.tag+"\n") {
			t.Fatalf("tag push not accepted: %q", output.Bytes())
		}
		if len(received) == 0 {
			t.Fatal("receive adapter was not called")
		}
		event := received[len(received)-1]
		if event.Update.Ref != "refs/tags/"+tc.tag || event.Update.NewSHA != object || event.ReleaseAdmission.SHA != commit {
			t.Fatalf("wrong durable receive: %+v", event)
		}
		run, err := db.LatestRunForRef(ctx, repo.ID, tc.tag)
		if err != nil {
			t.Fatal(err)
		}
		if run.SHA != object || run.TestedSHA != commit || run.Trigger != "tag" || !run.Release || run.Actor != "admin" {
			t.Fatalf("lost tag provenance: %+v", run)
		}

		if run.RefKind != model.RefTag || run.Ref != tc.tag {
			t.Fatalf("unexpected durable tag: %+v", run)
		}
		request := service.JobRequest{Run: run, Repository: repo, UpstreamName: "github", UpstreamOrg: "org", SourceDir: work, JobName: "push-" + tc.tag}
		if size, err := jobs.PipelineSize(ctx, request); err != nil || size != tc.size {
			t.Fatalf("%s size=%s err=%v", tc.tag, size, err)
		}
		if err := jobs.CreateRelease(ctx, request); err != nil {
			t.Fatal(err)
		}
		if control.request.Ref != tc.tag || control.request.PipelineFile != tc.file {
			t.Fatalf("wrong submission: %+v", control.request)
		}
		request.Run.RefKind = model.RefBranch
		if _, err := jobs.PipelineSize(ctx, request); err == nil {
			t.Fatal("branch kind accepted for release routing")
		}
	}
}

// Admission does not use Git; invoking one of these methods is a test failure.
// The actual pushed objects above supply the release request's provenance.
type unusedRouteGit struct{ service.WorkspaceGit }

// This fixture repository is already registered; accidental rediscovery must
// fail instead of silently replacing the identity used by route selection.
type registeredRouteDiscovery struct{}

func (registeredRouteDiscovery) DiscoverRepository(context.Context, string) (model.RepositorySpec, error) {
	return model.RepositorySpec{}, errors.New("fixture repository must already be registered")
}
