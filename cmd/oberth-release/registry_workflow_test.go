package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/pkg/argoworkflow"
	"helm.sh/helm/v3/pkg/chart/loader"
	corev1 "k8s.io/api/core/v1"
)

func TestReleasePushUsesGrantedRegistryLoginBeforePublication(t *testing.T) {
	wf, err := argoworkflow.Decode(ownWorkflow(t, "release"))
	if err != nil {
		t.Fatal(err)
	}
	dependencies := map[string]string{}
	for _, task := range wf.GetTemplateByName("release").DAG.Tasks {
		dependencies[task.Name] = task.Depends
	}
	if dependencies["release-images-pushed"] != "release-files-signed" || dependencies["release-published-immutable"] != "release-images-pushed" {
		t.Fatal("publication can run before signed images are pushed", dependencies)
	}
	for _, failure := range []string{"", "login", "first push"} {
		t.Run(failure, func(t *testing.T) { checkRegistryPushStep(t, wf, failure) })
	}
}

func TestReleasePushHasIsolatedFederatedIdentity(t *testing.T) {
	wf, err := argoworkflow.Decode(ownWorkflow(t, "release"))
	if err != nil {
		t.Fatal(err)
	}
	projections := 0
	for _, template := range wf.Spec.Templates {
		for _, volume := range template.Volumes {
			if volume.Projected == nil {
				continue
			}
			projections++
			assertRegistryIdentity(t, template, volume)
		}
	}
	if projections != 1 {
		t.Fatal("push has no isolated identity")
	}
}

func assertRegistryIdentity(t *testing.T, template wfv1.Template, volume corev1.Volume) {
	t.Helper()
	if template.Name != "release-images-pushed" || len(volume.Projected.Sources) != 1 {
		t.Fatal("registry identity crossed steps")
	}
	assertRegistryProjection(t, volume.Projected.Sources[0].ServiceAccountToken)
	mounts := 0
	for _, mount := range template.Container.VolumeMounts {
		if mount.Name == volume.Name {
			mounts++
			if !mount.ReadOnly || mount.MountPath != "/run/registry-identity" {
				t.Fatal("identity mount differs")
			}
		}
	}
	if mounts != 1 || template.Metadata.Annotations[argoworkflow.SecretPathsAnnotation] != "" {
		t.Fatal("push can use a stored login or lacks its identity mount")
	}
}

func assertRegistryProjection(t *testing.T, identity *corev1.ServiceAccountTokenProjection) {
	t.Helper()
	if identity == nil || identity.Audience != "oberth-release-registry" || identity.ExpirationSeconds == nil || *identity.ExpirationSeconds != 3600 || identity.Path != "jwt" {
		t.Fatal("registry identity differs")
	}
}

func checkRegistryPushStep(t *testing.T, wf *wfv1.Workflow, failure string) {
	t.Helper()
	dir := registryPushFixture(t)
	values := workflowValues(dir)
	values["DOCKER_CONFIG"] = filepath.Join(dir, "registry")
	values["PUSH_LOG"], values["PUSH_FAIL"] = filepath.Join(dir, "push.log"), failure
	output, err := executeWorkflowTemplate(t, wf, wf.GetTemplateByName("release-images-pushed"), values)
	if (err != nil) != (failure != "") || len(output) != 0 {
		t.Fatal("push failure or credential output was mishandled", err, string(output))
	}
	if failure == "login" {
		if _, err := os.Stat(values["PUSH_LOG"]); !os.IsNotExist(err) {
			t.Fatal("failed login reached image push")
		}
		return
	}
	body, err := os.ReadFile(values["PUSH_LOG"])
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(body)), "\n")
	if failure != "" {
		if len(lines) != 1 {
			t.Fatal("first push failure reached another push", lines)
		}
		return
	}
	assertRegistryPushTargets(t, dir, lines)
}

func registryPushFixture(t *testing.T) string {
	t.Helper()
	dir := workflowFixture(t)
	for _, image := range []string{"oberth", "oberth-source-plugin"} {
		if err := os.MkdirAll(filepath.Join(dir, "shared/dist", image+".oci"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	writeFixtureCommand(t, filepath.Join(dir, "shared/oberth-release"), `[ "$1" = registry-login ] || exit 2
[ "$PUSH_FAIL" != login ] || exit 1
mkdir -p "$DOCKER_CONFIG"
printf fixture-login > "$DOCKER_CONFIG/config.json"
`)
	writeFixtureCommand(t, filepath.Join(dir, "bin/go"), `[ "$1 $2 $3 $4" = 'tool -modfile=hack/tools/crane.mod crane push' ] || exit 2
[ "$(cat "$DOCKER_CONFIG/config.json")" = fixture-login ] || exit 3
[ -d "$5" ] || exit 4
printf '%s|%s\n' "$5" "$6" >> "$PUSH_LOG"
[ -z "$PUSH_FAIL" ]
`)
	return dir
}

func assertRegistryPushTargets(t *testing.T, dir string, lines []string) {
	t.Helper()
	chart, err := loader.LoadDir("../../charts/oberth")
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 2 {
		t.Fatal("did not push exactly two images", lines)
	}
	for i, image := range []struct{ name, field string }{{"oberth", "imageRepository"}, {"oberth-source-plugin", "sourcePluginImageRepository"}} {
		repository, ok := chart.Values[image.field].(string)
		if !ok || lines[i] != filepath.Join(dir, "shared/dist", image.name+".oci")+"|"+repository+":v1.2.3" {
			t.Fatal("push differs from signed-chart repository or layout", lines[i])
		}
	}
}

func TestReleaseSigningAndRegistryGrantsStaySeparate(t *testing.T) {
	r := ownRequest(ownWorkflow(t, "release"), "release", model.TriggerTag)
	r.Grants = releaseGrants(r)
	policy := ownPolicy()
	policy.Release = true
	hooks := ownHooks(t, r)
	called := false
	hooks.TransformNative = func(_ context.Context, _ *wfv1.Workflow, input admissioncontract.TransformInput) error {
		called = true
		if len(input.SecretGrants) != 3 {
			t.Fatal("secret delivery reached another step", input.SecretGrants)
		}
		for step, path := range map[string]string{"release-files-signed": "oberth/data/release/signing",
			"release-published-immutable": "oberth/data/release/github-token", "latest-advanced-monotonic": "oberth/data/release/github-token"} {
			grants := input.SecretGrants[step]
			if len(grants) != 1 || grants[0].Step != step || grants[0].Path != path {
				t.Fatal("secret delivery crossed step grants", step, grants)
			}
		}
		return nil
	}
	if _, err := argoworkflow.Prepare(t.Context(), r, policy, hooks); err != nil || !called {
		t.Fatal("release grant preparation failed", err)
	}
}

// The separation holds because each step declares its own path: a crossed
// grant is not delivered, and a missing one names its own step and path.
func TestReleaseRegistryLoginIsDeclaredByThePushStepOnly(t *testing.T) {
	r := ownRequest(ownWorkflow(t, "release"), "release", model.TriggerTag)
	r.Grants = releaseGrants(r)
	policy := ownPolicy()
	policy.Release = true
	crossed := r.Clone()
	login := crossed.Grants[1]
	login.Step = "release-files-signed"
	crossed.Grants = append(crossed.Grants, login)
	hooks := ownHooks(t, crossed)
	hooks.TransformNative = func(_ context.Context, _ *wfv1.Workflow, input admissioncontract.TransformInput) error {
		if signing := input.SecretGrants["release-files-signed"]; len(signing) != 1 || signing[0].Path != "oberth/data/release/signing" {
			t.Fatal("the signing step was given the registry login", signing)
		}
		return nil
	}
	if _, err := argoworkflow.Prepare(t.Context(), crossed, policy, hooks); err != nil {
		t.Fatal("an unused grant refused the release", err)
	}
	missing := r.Clone()
	missing.Grants = slices.Delete(slices.Clone(missing.Grants), 1, 2)
	want := "oberth access allow --kind identity --audience 'oberth-release-registry' '" + r.Repository + "' '.oberth/release.yaml' 'release-images-pushed' 'identity'"
	if finding := releaseRefusal(t, missing, policy); finding.Command != want {
		t.Fatal("a missing registry login does not print its own grant command", finding)
	}
}

func releaseRefusal(t *testing.T, r admissioncontract.PrepareRequest, policy argoworkflow.Policy) admissioncontract.Finding {
	t.Helper()
	_, err := argoworkflow.Prepare(t.Context(), r, policy, ownHooks(t, r))
	var refused *argoworkflow.AdmissionError
	if !errors.As(err, &refused) || len(refused.Findings) != 1 {
		t.Fatal("release preparation did not give one refusal", err)
	}
	return refused.Findings[0]
}
