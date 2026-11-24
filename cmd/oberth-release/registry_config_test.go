package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/pkg/argoworkflow"
)

func TestReleaseMissingRegistryConfigRefusesBeforePush(t *testing.T) {
	r := ownRequest(ownWorkflow(t, "release"), "release", model.TriggerTag)
	r.Grants = releaseGrants(r)
	policy := ownPolicy()
	policy.Release = true
	prepared, err := argoworkflow.Prepare(t.Context(), r, policy, ownHooks(t, r))
	if err != nil {
		t.Fatal("optional registry configuration was not admitted", err)
	}
	assertOptionalRegistryConfig(t, prepared.View().Workflow)
	wf, err := argoworkflow.Decode(r.Document)
	if err != nil {
		t.Fatal(err)
	}
	binary := releaseHelpBinary(t)
	for _, missing := range []string{"RELEASE_WIF_PROVIDER", "RELEASE_WIF_ACCOUNT", "both"} {
		t.Run(missing, func(t *testing.T) {
			checkMissingRegistryConfig(t, wf, binary, missing)
		})
	}
}

func assertOptionalRegistryConfig(t *testing.T, wf *wfv1.Workflow) {
	t.Helper()
	refs := map[string]string{}
	for _, env := range wf.GetTemplateByName("release-images-pushed").Container.Env {
		if env.Name != "RELEASE_WIF_PROVIDER" && env.Name != "RELEASE_WIF_ACCOUNT" {
			continue
		}
		if env.ValueFrom == nil || env.ValueFrom.ConfigMapKeyRef == nil {
			t.Fatal("registry configuration lost its ConfigMap reference", env.Name)
		}
		ref := env.ValueFrom.ConfigMapKeyRef
		if ref.Name != "release-config" || ref.Optional == nil || !*ref.Optional {
			t.Fatal("missing registry configuration prevents the helper from starting", env.Name)
		}
		refs[env.Name] = ref.Key
	}
	if refs["RELEASE_WIF_PROVIDER"] != "registry-provider" || refs["RELEASE_WIF_ACCOUNT"] != "registry-account" {
		t.Fatal("registry configuration references differ", refs)
	}
}

func checkMissingRegistryConfig(t *testing.T, wf *wfv1.Workflow, binary, missing string) {
	t.Helper()
	dir := workflowFixture(t)
	if err := os.Symlink(binary, filepath.Join(dir, "shared/oberth-release")); err != nil {
		t.Fatal(err)
	}
	writeFixtureCommand(t, filepath.Join(dir, "bin/go"), `printf push > "$FIXTURE_ROOT/push.log"`)
	values := workflowValues(dir)
	values["DOCKER_CONFIG"] = filepath.Join(dir, "registry")
	values["RELEASE_WIF_PROVIDER"] = "projects/123/locations/global/workloadIdentityPools/pool/providers/provider"
	values["RELEASE_WIF_ACCOUNT"] = "writer@project.iam.gserviceaccount.com"
	if missing == "both" {
		delete(values, "RELEASE_WIF_PROVIDER")
		delete(values, "RELEASE_WIF_ACCOUNT")
	} else {
		delete(values, missing)
	}
	output, err := executeWorkflowTemplate(t, wf, wf.GetTemplateByName("release-images-pushed"), values)
	if err == nil || !strings.Contains(string(output), federationConfig.Error()) || !strings.Contains(string(output), federationConfig.Remedy()) {
		t.Fatal("missing registry configuration did not refuse at the config stage", err, string(output))
	}
	for _, path := range []string{filepath.Join(dir, "push.log"), values["DOCKER_CONFIG"]} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatal("missing registry configuration reached login or push", path, err)
		}
	}
}
