//go:build linux

// These tests exercise the Linux server and Git receive path.

package main

import (
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/gitcache/gittest"
	"github.com/oberthci/oberth/internal/identityname"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestComposedReleaseAccountUsesVerifiedTagAndAudience(t *testing.T) {
	f := composeFixture(t, composeBinary(t))
	f.prepareCommit(t)
	document := strings.Replace(composedWorkflow, "    container:\n", `    volumes:
    - name: identity
      projected:
        sources:
        - serviceAccountToken: {audience: publisher, expirationSeconds: 600, path: token}
    container:
      volumeMounts: [{name: identity, mountPath: /run/identity, readOnly: true}]
`, 1)
	f.git.Write(t, ".oberth/release.yaml", []byte(document))
	commit := f.git.CommitFile(t, "release projection")
	f.git.Git(t, f.git.Work, "push", f.git.Bare, "HEAD:refs/heads/main")
	f.prepareAdministrator(t)
	cancel, done := f.run(t)
	defer cancel()
	f.checkStarting(t, done)
	f.waitStarted(t, done)
	f.bindClient(t)
	app, _, diagnostic := f.revokeApplication(t)
	args := []string{"access", "allow", "--kind", "identity", "--audience", "publisher", gittest.Repository, ".oberth/release.yaml", "build", "identity"}
	if code := app.run(t.Context(), args); code != 0 {
		t.Fatal("audience grant command failed", code, diagnostic.String())
	}
	f.releaseBuild(t, "refs/heads/tested", true)
	f.git.Git(t, f.git.Work, "tag", "v1.2.3", commit)
	f.git.Git(t, f.git.Work, "push", "oberth", "refs/tags/v1.2.3")
	run := f.waitMatchingRun(t, func(r stateport.Run) bool { return r.Ref == "refs/tags/v1.2.3" && r.Origin.WorkflowUID != "" })
	native, err := f.argo.ArgoprojV1alpha1().Workflows(f.config.PipelineNamespace).Get(t.Context(), run.Origin.WorkflowName, meta.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	want, err := identityname.Name(identityname.Key{Repository: gittest.Repository, File: ".oberth/release.yaml", Step: "build"}, identityname.Step)
	if err != nil || native.Spec.Templates[0].ServiceAccountName != want {
		t.Fatal("release account differs", err)
	}
	f.checkOrdinaryTagAuthority(t, commit)
	f.git.Write(t, ".oberth/build.yaml", []byte(document))
	f.git.CommitFile(t, "projection on branch")
	if pushed, err := f.pushResult(t, "refs/heads/audience", "-o", "oberth.run=1"); err == nil {
		t.Fatal("branch file bypassed its audience grant", pushed)
	}
	f.checkUnverifiedTagAuthority(t)
}

func (f *composedFixture) checkOrdinaryTagAuthority(t *testing.T, commit string) {
	t.Helper()
	q := service.StartRequest{Request: model.CIRequest{Repo: gittest.Repository, Ref: "refs/tags/ordinary", ObjectSHA: commit, CommitSHA: &commit}}
	resolved, err := f.owners.ResolveStart(t.Context(), q)
	if err != nil || resolved.Release {
		t.Fatal("ordinary tag gained release authority on inspection or rerun", err)
	}
}

func (f *composedFixture) checkUnverifiedTagAuthority(t *testing.T) {
	t.Helper()
	f.git.Git(t, f.git.Work, "tag", "v1.2.4")
	if pushed, err := f.pushResult(t, "refs/tags/v1.2.4"); !strings.Contains(pushed, "outside default history") {
		t.Fatal("unverified release did not explain its refusal", pushed, err)
	}
	runs, err := f.owners.c.store.Runs(t.Context(), stateport.RunFilter{Ref: "refs/tags/v1.2.4", Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	for _, run := range runs {
		if run.Release || run.Status != stepmodel.RunFailed || run.Origin.WorkflowUID != "" {
			t.Fatal("unverified release gained native authority", run)
		}
	}
	request := service.StartRequest{Request: model.CIRequest{Repo: gittest.Repository, Ref: "refs/tags/v1.2.4", Trigger: model.TriggerTag}}
	resolved, err := f.owners.ResolveStart(t.Context(), request)
	if err == nil || resolved.Release || publicerr.Classify(err).Code() != publicerr.NotFound {
		t.Fatal("refused tag was retained or gained release authority", resolved.Release, err)
	}
}
