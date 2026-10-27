package argojob

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"

	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/periapsis"
)

func genericWIFConfig() Config {
	c := wifTestConfig()
	c.ReleaseWIF.Roles["artifact-deployer"] = c.ReleaseWIF.Roles["image-writer"]
	delete(c.ReleaseWIF.Roles, "image-writer")
	grant := c.ReleaseWIF.Repositories["github/skipops/oberth"]
	grant.Templates["main"] = "artifact-deployer"
	grant.StepServiceAccounts = map[string]string{"main": "release-step-main"}
	grant.StepRoutes = map[string]ReleaseTagRoute{"main": {Pattern: "v*", File: argoworkflow.ReleaseFile}}
	c.ReleaseTagRoutes = ReleaseTagRoutes{"github/skipops/oberth": {grant.StepRoutes["main"]}}
	c.ReleaseWIF.Repositories["github/skipops/oberth"] = grant
	return c
}

func genericWIFRequest(t *testing.T, mutate func(*wfv1.Workflow)) Request {
	t.Helper()
	r := wifTestRequest(t, func(wf *wfv1.Workflow) {
		wf.Spec.Templates[0].Metadata.Annotations[argoworkflow.ReleaseWIFRoleAnnotation] = "artifact-deployer"
		if mutate != nil {
			mutate(wf)
		}
	})
	r.Ref = "v1"
	return r
}

func TestGenericWIFOnlyIdentityWithoutSecretGrant(t *testing.T) {
	c := genericWIFConfig()
	r := genericWIFRequest(t, func(wf *wfv1.Workflow) {
		sibling := wf.Spec.Templates[0].DeepCopy()
		sibling.Name = "sibling"
		sibling.Metadata.Annotations = nil
		sibling.ServiceAccountName = "release-step-main"
		wf.Spec.Templates = append(wf.Spec.Templates, *sibling)
	})
	if len(r.ApprovedSecrets) != 0 || len(r.StepGrants) != 0 || len(c.PerStepIdentities) != 0 {
		t.Fatal("fixture must not have a dummy secret grant")
	}
	wf, err := Build(c, r)
	if err != nil {
		t.Fatal(err)
	}
	if wf.Spec.Templates[0].ServiceAccountName != "release-step-main" || wf.Spec.Templates[1].ServiceAccountName == "release-step-main" {
		t.Fatalf("identity escaped mapped leaf: %+v", wf.Spec.Templates)
	}
	for _, v := range templateMainContainer(&wf.Spec.Templates[0]).Env {
		if strings.HasPrefix(v.Name, "VAULT_") || v.Name == "OBERTH_VAULT_ROLE" {
			t.Fatalf("WIF-only leaf got Vault wiring: %s", v.Name)
		}
	}
	preflight, err := PreflightRelease(c, r)
	if err != nil || len(preflight.Problems) != 0 || len(preflight.Declarations) != 0 || !strings.Contains(strings.Join(preflight.Checks, "\n"), "system:serviceaccount:"+c.Namespace+":release-step-main") {
		t.Fatalf("preflight %+v err=%v", preflight, err)
	}
	// Removing WIF on a branch cannot leave an administrator-only step identity.
	var source wfv1.Workflow
	if err := json.Unmarshal(r.Source, &source); err != nil {
		t.Fatal(err)
	}
	delete(source.Spec.Templates[0].Metadata.Annotations, argoworkflow.ReleaseWIFRoleAnnotation)
	source.Spec.Templates[0].ServiceAccountName = "release-step-main"
	r.Source, _ = json.Marshal(source)
	r.Trigger = periapsis.TriggerCI
	r.RunTrigger = argoworkflow.RunTriggerBranch
	r.Ref = "refs/heads/artifact-deploy-test"
	branch, err := Build(c, r)
	if err != nil {
		t.Fatal(err)
	}
	for _, leaf := range branch.Spec.Templates {
		if leaf.ServiceAccountName == "release-step-main" {
			t.Fatal("branch acquired release identity")
		}
		for _, v := range leaf.Volumes {
			if v.Name == releaseWIFVolume {
				t.Fatal("branch got WIF projection")
			}
		}
	}
}

func TestGenericWIFRefusesIdentityMisuse(t *testing.T) {
	for _, kind := range []string{"branch", "promotion", "schedule", "unknown-role", "unmapped-role", "invalid-role", "sibling", "repo-sa", "other-repo-sa", "ci-sa", "shared-sa", "other-step", "pinned-other-step", "unmapped-step", "secretstore"} {
		t.Run(kind, func(t *testing.T) {
			c := genericWIFConfig()
			r := genericWIFRequest(t, func(wf *wfv1.Workflow) {
				switch kind {
				case "unknown-role":
					wf.Spec.Templates[0].Metadata.Annotations[argoworkflow.ReleaseWIFRoleAnnotation] = "not-configured"
				case "unmapped-role":
					wf.Spec.Templates[0].Metadata.Annotations[argoworkflow.ReleaseWIFRoleAnnotation] = "chart-writer"
				case "invalid-role":
					wf.Spec.Templates[0].Metadata.Annotations[argoworkflow.ReleaseWIFRoleAnnotation] = "Bad/Role"
				case "sibling":
					wf.Spec.Templates[0].Name = "sibling"
					wf.Spec.Entrypoint = "sibling"
				case "secretstore":
					wf.Spec.Templates[0].Container.Command = []string{"oberth", "secretstore", "exec", "--path", "oberth/data/release/signing", "--", "true"}
				}
			})
			grant := c.ReleaseWIF.Repositories["github/skipops/oberth"]
			switch kind {
			case "branch":
				r.Trigger = periapsis.TriggerCI
				r.RunTrigger = argoworkflow.RunTriggerBranch
			case "promotion":
				r.Trigger = periapsis.TriggerCI
				r.RunTrigger = argoworkflow.RunTriggerPromotion
			case "schedule":
				r.Trigger = periapsis.TriggerCI
				r.RunTrigger = argoworkflow.RunTriggerSchedule
			case "repo-sa":
				grant.StepServiceAccounts["main"] = grant.ServiceAccountName
			case "other-repo-sa":
				c.PerRepoIdentities["github/org/foreign"] = PerRepoIdentityConfig{ServiceAccountName: "release-step-main"}
			case "ci-sa":
				c.PerRepoCIIdentities = map[string]PerRepoIdentityConfig{"github/org/repo": {ServiceAccountName: "release-step-main"}}
			case "shared-sa":
				grant.StepServiceAccounts["main"] = c.PipelineServiceAccount
			case "other-step":
				c.PerStepIdentities = map[string]PerRepoIdentityConfig{"github/skipops/oberth/sibling": {ServiceAccountName: "release-step-main"}}
			case "pinned-other-step":
				grant.Templates["sibling"] = "artifact-deployer"
				grant.StepServiceAccounts["sibling"] = "release-step-main"
			case "unmapped-step":
				grant.StepServiceAccounts["sibling"] = "other-step-sa"
			}
			c.ReleaseWIF.Repositories["github/skipops/oberth"] = grant
			if _, err := Build(c, r); err == nil {
				t.Fatal("misuse admitted")
			}
		})
	}
}

func TestGenericWIFRoleBoundsAndSnapshot(t *testing.T) {
	c := genericWIFConfig()
	clone := c.ReleaseWIF.Clone()
	grant := c.ReleaseWIF.Repositories["github/skipops/oberth"]
	grant.StepServiceAccounts["main"] = "changed"
	if clone.Repositories["github/skipops/oberth"].StepServiceAccounts["main"] != "release-step-main" {
		t.Fatal("pinned mapping aliased caller")
	}
	c.ReleaseWIF = clone
	for i := 0; len(c.ReleaseWIF.Roles) < MaxReleaseWIFRoles; i++ {
		role := fmt.Sprintf("role-%d", i)
		c.ReleaseWIF.Roles[role] = ReleaseWIFRole{Provider: "projects/123/locations/global/workloadIdentityPools/" + role + "/providers/issuer", ServiceAccount: role + "@example-project.iam.gserviceaccount.com"}
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	c.ReleaseWIF.Roles["overflow"] = ReleaseWIFRole{Provider: "projects/123/locations/global/workloadIdentityPools/overflow/providers/issuer", ServiceAccount: "overflow@example-project.iam.gserviceaccount.com"}
	if err := c.Validate(); err == nil {
		t.Fatal("unbounded role count")
	}
}

func TestGenericWIFRepositoryWithoutAnySecretDerivedIdentity(t *testing.T) {
	c := genericWIFConfig()
	c.PerRepoIdentities = nil
	c.PerRepoCIIdentities = nil
	c.PerStepIdentities = nil
	grant := c.ReleaseWIF.Repositories["github/skipops/oberth"]
	grant.ServiceAccountName = ""
	c.ReleaseWIF.Repositories["github/skipops/oberth"] = grant
	r := genericWIFRequest(t, nil)
	wf, err := Build(c, r)
	if err != nil {
		t.Fatal(err)
	}
	if wf.Spec.ServiceAccountName != c.PipelineServiceAccount || wf.Spec.Templates[0].ServiceAccountName != "release-step-main" {
		t.Fatal("WIF-only repository still requires a secret-derived identity")
	}
	result, err := PreflightRelease(c, r)
	if err != nil || len(result.Problems) != 0 {
		t.Fatal(result, err)
	}
}

func TestWIFStepIdentityIsBoundToExactRoute(t *testing.T) {
	for _, kind := range []string{"default-file", "other-file", "other-pattern-same-file", "unmatched-tag", "full-ref", "missing-route", "wrong-kind", "missing-binding", "orphan-binding"} {
		t.Run(kind, func(t *testing.T) {
			c := genericWIFConfig()
			r := genericWIFRequest(t, nil)
			grant := c.ReleaseWIF.Repositories["github/skipops/oberth"]
			pin := ReleaseTagRoute{Pattern: "demo-*", File: ".oberth/demo.yaml"}
			grant.StepRoutes["main"] = pin
			c.ReleaseTagRoutes["github/skipops/oberth"] = []ReleaseTagRoute{pin, {Pattern: "other-*", File: pin.File}}
			r.Ref, r.PipelineFile = "demo-1", pin.File
			switch kind {
			case "default-file":
				r.Ref, r.PipelineFile = "v1", argoworkflow.ReleaseFile
			case "other-file":
				r.PipelineFile = ".oberth/other.yaml"
			case "other-pattern-same-file":
				r.Ref = "other-1"
			case "unmatched-tag":
				r.Ref = "v1"
			case "full-ref":
				r.Ref = "refs/tags/demo-1"
			case "missing-route":
				c.ReleaseTagRoutes = nil
			case "wrong-kind":
				r.RunTrigger = argoworkflow.RunTriggerBranch
			case "missing-binding":
				delete(grant.StepRoutes, "main")
			case "orphan-binding":
				grant.StepRoutes["other"] = pin
			}
			c.ReleaseWIF.Repositories["github/skipops/oberth"] = grant
			if _, err := Build(c, r); err == nil {
				t.Fatal("dedicated identity escaped its exact file/pattern binding")
			}
		})
	}
	c := genericWIFConfig()
	r := genericWIFRequest(t, nil)
	preflight := r
	preflight.Ref = ""
	result, err := PreflightRelease(c, preflight)
	if err != nil || len(result.Problems) > 0 {
		t.Fatalf("read-only file preflight: %+v %v", result, err)
	}
	// The preflight bypass is local, private and cannot mutate a caller into an
	// admission-capable request without its own real tag binding.
	if _, err := Build(c, preflight); err == nil {
		t.Fatal("preflight authorized delivery without a tag")
	}
	snapshot := c.ReleaseWIF.Clone()
	c.ReleaseWIF.Repositories["github/skipops/oberth"].StepRoutes["main"] = ReleaseTagRoute{Pattern: "*", File: ".oberth/other.yaml"}
	if snapshot.Repositories["github/skipops/oberth"].StepRoutes["main"].File != argoworkflow.ReleaseFile {
		t.Fatal("route bindings alias caller")
	}
}
