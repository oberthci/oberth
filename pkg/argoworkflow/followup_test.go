package argoworkflow

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	eventsv1 "github.com/argoproj/argo-events/pkg/apis/events/v1alpha1"
	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/model"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

func followupBoundaryChecks() []boundaryCheck {
	rows := []boundaryCheck{
		{"identity audience grant", "grant", `spec.templates["main"].volumes[login].audience[login]`, tokenDocument, func(d *checkDocument) { d.request.Grants = nil }},
		{"PVC repository grant", "pvc", `spec.templates["main"].volumes[v]`, pvcDocument, func(d *checkDocument) { d.policy.PVCClaims = nil }},
	}
	for _, group := range sortedKeys(argoAPIKinds()) {
		for _, kind := range append(argoAPIKinds()[group], "FutureArgoKind") {
			for _, version := range []string{"v1alpha1", "v99"} {
				rows = append(rows, boundaryCheck{"Argo API " + kind + "/" + version, "workload_kind", "policy.capability.resourceKinds[" + kind + "]", resourceDocument, func(d *checkDocument) {
					api := group + "/" + version
					d.policy.Capability.ResourceKinds = []ResourceKind{{APIVersion: api, Kind: kind, Actions: []string{"create"}}}
					d.template().Resource.Manifest = "apiVersion: " + api + "\nkind: " + kind + "\nmetadata: {name: child, namespace: resources}"
				}})
			}
		}
	}
	return rows
}
func pvcDocument(d *checkDocument) {
	d.volume(corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "data"}})
	d.policy.PVCClaims = map[string]bool{"data": true}
}

func TestIdentityAndPVCRecipesAdmitTheSameDocument(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, audience string
		release        bool
	}{
		{"identity login", "login", false},
		{"identity URL", "https://identity.example.test", false},
		{"PVC CI", "", false},
		{"PVC release", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := checkDocument{wf: simpleWorkflow(), request: requestFor(t, simpleWorkflow()), hooks: identityHooks()}
			var want string
			if tc.audience != "" {
				tokenDocument(&d)
				d.hooks.LoginAudience = tc.audience
				d.template().Volumes[0].Projected.Sources[0].ServiceAccountToken.Audience = tc.audience
				d.request.Grants = nil
				want = "oberth access allow --kind identity --audience '" + tc.audience + "' 'forge/org/repo' '.oberth/build.yaml' 'main' 'identity'"
			} else {
				pvcDocument(&d)
				d.policy.PVCClaims = nil
				d.policy.Release = tc.release
				tier := "ci"
				if tc.release {
					tier = "release"
					d.request.Trigger = model.TriggerTag
				}
				want = "oberth access allow --kind pvc --tier " + tier + " 'forge/org/repo' 'data'"
			}
			d.sync(t)
			_, err := Prepare(t.Context(), d.request, d.policy, d.hooks)
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatal("missing exact command", err, want)
			}
			assertFollowupGrantMessage(t, err, tc.audience)
			applyFollowupRecipe(t, &d, want)
			d.sync(t)
			if _, err := Prepare(t.Context(), d.request, d.policy, d.hooks); err != nil {
				t.Fatal("printed grant did not admit same document", err)
			}
		})
	}
}
func assertFollowupGrantMessage(t *testing.T, err error, audience string) {
	t.Helper()
	findings := admissionFindings(t, err)
	if len(findings) != 1 {
		t.Fatal(err)
	}
	if audience == "" && (findings[0].Code != "pvc" || findings[0].Reason != "PVC has no repository grant" || strings.Contains(findings[0].Remedy, "audience")) {
		t.Fatal(err)
	}
}
func applyFollowupRecipe(t *testing.T, d *checkDocument, command string) {
	t.Helper()
	quoted := strings.Split(command, "'")
	flags := strings.Fields(quoted[0])
	switch flags[4] {
	case "identity":
		if len(quoted) != 11 || flags[5] != "--audience" {
			t.Fatal(command)
		}
		d.request.Grants = []admissioncontract.GrantEvidence{{Kind: "identity", Audience: quoted[1], Repository: quoted[3], File: quoted[5], Step: quoted[7], Path: quoted[9], State: admissioncontract.StateVerified, Revision: "approval"}}
	case "pvc":
		if len(quoted) != 5 || flags[5] != "--tier" || quoted[1] != d.request.Repository || (flags[6] == "release") != d.policy.Release {
			t.Fatal(command)
		}
		d.policy.PVCClaims = map[string]bool{quoted[3]: true}
	default:
		t.Fatal(command)
	}
}

func TestMissingResourceNamespaceHasItsOwnMessage(t *testing.T) {
	t.Parallel()
	for _, namespace := range []string{"", ", namespace: ''"} {
		w, p := resourceWorkflow("apiVersion: example.test/v1\nkind: Widget\nmetadata: {name: child" + namespace + "}")
		r := requestFor(t, w)
		r.IdentityRequest.ResourceAccount = "resources"
		_, err := Prepare(t.Context(), r, p.Policy, identityHooks())
		findings := admissionFindings(t, err)
		if len(findings) != 1 || findings[0].Code != "resource_namespace" || findings[0].Reason != "resource manifest has no namespace" || findings[0].Remedy != "Set metadata.namespace explicitly to the granted deployment namespace." {
			t.Fatal(err)
		}
	}
}

func TestSharedPullSecretsUseEveryConsumerStepGrant(t *testing.T) {
	t.Parallel()
	w := sharedConsumer("shared", "work")
	w.Spec.ImagePullSecrets = []corev1.LocalObjectReference{{Name: "private"}}
	w.Spec.Templates[0].DAG.Tasks = append(w.Spec.Templates[0].DAG.Tasks, wfv1.DAGTask{Name: "other", TemplateRef: &wfv1.TemplateRef{Name: "shared", Template: "other"}})
	shared := sharedObject("shared")
	second := *shared.Spec.Templates[0].DeepCopy()
	second.Name = "other"
	shared.Spec.Templates = append(shared.Spec.Templates, second)
	// The provider's pull list is not used to create the consumer's Pods.
	shared.Spec.ImagePullSecrets = []corev1.LocalObjectReference{{Name: "provider-only"}}
	policy := Policy{TemplateGetter: getterFor(map[string]*wfv1.WorkflowTemplate{"shared": shared})}
	r := requestFor(t, w)
	r.Grants = []admissioncontract.GrantEvidence{clusterGrant("work", "image-pull-secret", "private"), clusterGrant("other", "image-pull-secret", "private")}
	if _, err := Prepare(t.Context(), r, policy, identityHooks()); err != nil {
		t.Fatal("consumer's exact grants refused", err)
	}
	for _, field := range []string{"repo", "file", "step", "path", "kind", "state", "revision"} {
		t.Run(field, func(t *testing.T) {
			bad := r.Clone()
			wrongGrant(&bad.Grants[1], field)
			if field == "step" {
				bad.Grants[1].Step = "unapproved"
			}
			_, err := Prepare(t.Context(), bad, policy, identityHooks())
			requireCode(t, err, "read")
			if !strings.Contains(err.Error(), "--kind image_pull_secret 'forge/org/repo' '.oberth/build.yaml' 'other' 'private'") {
				t.Fatal(err)
			}
		})
	}
	r.Grants = []admissioncontract.GrantEvidence{clusterGrant("*", "image-pull-secret", "private")}
	if _, err := Prepare(t.Context(), r, policy, identityHooks()); err != nil {
		t.Fatal("wildcard consumer grant refused", err)
	}
}

func TestOfflinePVCGrantDeferralIsExplicit(t *testing.T) {
	t.Parallel()
	d := checkDocument{wf: simpleWorkflow(), request: requestFor(t, simpleWorkflow()), hooks: identityHooks()}
	pvcDocument(&d)
	d.policy.PVCClaims = nil
	d.request.Mode = admissioncontract.ModeInspect
	d.request.IdentityRequest.Mode = admissioncontract.ModeInspect
	d.sync(t)
	prepared, err := Prepare(t.Context(), d.request, d.policy, d.hooks)
	if err != nil {
		t.Fatal("offline grant was turned into a hard refusal", err)
	}
	for _, finding := range prepared.View().Findings {
		if finding.Code == "pvc" {
			return
		}
	}
	t.Fatal("offline missing grant disappeared")
}

func TestGeneratedArgoGroupsMatchPinnedAPIRegistration(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{wfv1.AddToScheme, eventsv1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	groups := map[string][]string{}
	for kind, typ := range scheme.AllKnownTypes() {
		if strings.HasPrefix(typ.PkgPath(), "github.com/argoproj/") && !strings.HasSuffix(kind.Kind, "List") {
			groups[kind.Group] = append(groups[kind.Group], kind.Kind)
		}
	}
	for _, kinds := range groups {
		slices.Sort(kinds)
	}
	if !reflect.DeepEqual(groups, argoAPIKinds()) {
		t.Fatal("regenerate API kinds", groups, argoAPIKinds())
	}
}
