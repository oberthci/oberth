package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"

	jsonpatch "gopkg.in/evanphx/json-patch.v4"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/managedfields"
	"k8s.io/apimachinery/pkg/util/managedfields/managedfieldstest"
	"k8s.io/client-go/kubernetes/fake"
	rbacclient "k8s.io/client-go/kubernetes/typed/rbac/v1"
	"k8s.io/client-go/rest"
	ktesting "k8s.io/client-go/testing"
)

func fixtureRole(t *testing.T, profile string) (*rbacv1.Role, target) {
	t.Helper()
	variant := "default"
	if profile != "" {
		variant = "nonroot"
	}
	b, err := os.ReadFile("../../cmd/oberth/testdata/argo-role-rules-v0.17.9-" + variant + ".json")
	if err != nil {
		t.Fatal(err)
	}
	r := &rbacv1.Role{TypeMeta: metav1.TypeMeta{APIVersion: rbacv1.SchemeGroupVersion.String(), Kind: "Role"}, ObjectMeta: metav1.ObjectMeta{
		Name: "oberth-argo", Namespace: "pipelines", UID: "role-uid", ResourceVersion: "17",
		Labels:      map[string]string{"app.kubernetes.io/managed-by": "Helm", "keep": "yes"},
		Annotations: map[string]string{"meta.helm.sh/release-name": "oberth", "meta.helm.sh/release-namespace": "server"},
	}}
	if err := json.Unmarshal(b, &r.Rules); err != nil {
		t.Fatal(err)
	}
	r.ManagedFields = []metav1.ManagedFieldsEntry{
		{Manager: "helm", Operation: metav1.ManagedFieldsOperationApply, APIVersion: r.APIVersion, FieldsType: "FieldsV1", FieldsV1: &metav1.FieldsV1{Raw: []byte(`{"f:metadata":{"f:labels":{"f:app.kubernetes.io/managed-by":{}}}}`)}},
		{Manager: "kubectl-patch", Operation: metav1.ManagedFieldsOperationUpdate, APIVersion: r.APIVersion, FieldsType: "FieldsV1", FieldsV1: &metav1.FieldsV1{Raw: []byte(`{"f:rules":{},"f:metadata":{"f:labels":{"f:keep":{}}}}`)}},
	}
	return r, target{Namespace: "pipelines", Name: "oberth-argo", Release: "oberth", ReleaseNamespace: "server", CredentialedSA: "oberth-argo-credentialed", Profile: profile}
}

func patchRole(t *testing.T, role *rbacv1.Role, patch []byte) *rbacv1.Role {
	t.Helper()
	p, err := jsonpatch.DecodePatch(patch)
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(role)
	if err != nil {
		t.Fatal(err)
	}
	b, err = p.Apply(b)
	if err != nil {
		t.Fatal(err)
	}
	var result rbacv1.Role
	if err := json.Unmarshal(b, &result); err != nil {
		t.Fatal(err)
	}
	return &result
}

func TestMigrationTransfersOnlyRulesAndIsIdempotent(t *testing.T) {
	r, spec := fixtureRole(t, "")
	before := r.DeepCopy()
	p, err := migrationPatch(r, spec)
	if err != nil || len(p) == 0 {
		t.Fatalf("patch=%s err=%v", p, err)
	}
	after := patchRole(t, r, p)
	if !reflect.DeepEqual(r, before) {
		t.Fatal("planner mutated input")
	}
	if !reflect.DeepEqual(after.Rules, r.Rules) {
		t.Fatal("permissions changed")
	}
	fields := after.ManagedFields
	after.ManagedFields = before.ManagedFields
	if !reflect.DeepEqual(after, before) {
		t.Fatal("content other than managedFields changed")
	}
	if !strings.Contains(string(fields[0].FieldsV1.Raw), `"f:rules":{}`) || strings.Contains(string(fields[1].FieldsV1.Raw), "f:rules") || !strings.Contains(string(fields[1].FieldsV1.Raw), "f:keep") {
		t.Fatalf("wrong ownership: %+v", fields)
	}
	after.ManagedFields = fields
	if again, err := migrationPatch(after, spec); err != nil || len(again) != 0 {
		t.Fatalf("repeat=%s err=%v", again, err)
	}
}

func TestMigrationRefusesUnreviewedShape(t *testing.T) {
	cases := map[string]func(*rbacv1.Role){
		"uid missing":             func(r *rbacv1.Role) { r.UID = "" },
		"version missing":         func(r *rbacv1.Role) { r.ResourceVersion = "" },
		"wrong name":              func(r *rbacv1.Role) { r.Name = "different" },
		"wrong namespace":         func(r *rbacv1.Role) { r.Namespace = "different" },
		"wrong release":           func(r *rbacv1.Role) { r.Annotations["meta.helm.sh/release-name"] = "different" },
		"wrong release namespace": func(r *rbacv1.Role) { r.Annotations["meta.helm.sh/release-namespace"] = "different" },
		"not Helm":                func(r *rbacv1.Role) { r.Labels["app.kubernetes.io/managed-by"] = "other" },
		"deleting":                func(r *rbacv1.Role) { now := metav1.Now(); r.DeletionTimestamp = &now },
		"extra permission":        func(r *rbacv1.Role) { r.Rules[0].Verbs = append(r.Rules[0].Verbs, "*") },
		"missing permission":      func(r *rbacv1.Role) { r.Rules = r.Rules[1:] },
		"unknown rules owner":     func(r *rbacv1.Role) { r.ManagedFields[1].Manager = "operator" },
		"wrong API version":       func(r *rbacv1.Role) { r.ManagedFields[1].APIVersion = "v1" },
		"subresource":             func(r *rbacv1.Role) { r.ManagedFields[1].Subresource = "status" },
		"wrong operation":         func(r *rbacv1.Role) { r.ManagedFields[1].Operation = "bogus" },
		"wrong fields type":       func(r *rbacv1.Role) { r.ManagedFields[1].FieldsType = "bogus" },
		"malformed fields":        func(r *rbacv1.Role) { r.ManagedFields[1].FieldsV1.Raw = []byte(`{"f:rules":false}`) },
		"granular rules":          func(r *rbacv1.Role) { r.ManagedFields[1].FieldsV1.Raw = []byte(`{"f:rules":{"i:0":{}}}`) },
		"untracked rules":         func(r *rbacv1.Role) { r.ManagedFields = r.ManagedFields[:1] },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			r, spec := fixtureRole(t, "")
			change(r)
			if p, err := migrationPatch(r, spec); err == nil || len(p) != 0 {
				t.Fatalf("accepted: %s %v", p, err)
			}
		})
	}
}

func TestMigrationPatchRejectsConcurrentChange(t *testing.T) {
	for _, field := range []string{"uid", "resourceVersion", "rules", "managedFields"} {
		t.Run(field, func(t *testing.T) {
			r, spec := fixtureRole(t, "")
			p, err := migrationPatch(r, spec)
			if err != nil {
				t.Fatal(err)
			}
			switch field {
			case "uid":
				r.UID = "recreated"
			case "resourceVersion":
				r.ResourceVersion = "18"
			case "rules":
				r.Rules[0].Verbs = []string{"get"}
			case "managedFields":
				r.ManagedFields[1].Manager = "different"
			}
			b, _ := json.Marshal(r)
			decoded, _ := jsonpatch.DecodePatch(p)
			if _, err := decoded.Apply(b); err == nil {
				t.Fatal("stale patch succeeded")
			}
		})
	}
}

func asUnstructured(t *testing.T, r *rbacv1.Role) *unstructured.Unstructured {
	t.Helper()
	m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(r)
	if err != nil {
		t.Fatal(err)
	}
	return &unstructured.Unstructured{Object: m}
}

func trackedRole(t *testing.T, fm managedfieldstest.TestFieldManager) *rbacv1.Role {
	t.Helper()
	r := &rbacv1.Role{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(fm.Live().(*unstructured.Unstructured).Object, r); err != nil {
		t.Fatal(err)
	}
	return r
}

// Use Kubernetes' real field ownership engine: fake clientsets alone cannot
// prove that an atomic Role.rules list can be upgraded without force.
func TestMigrationUpgradeFromCurrentLiveShape(t *testing.T) {
	for _, profile := range []string{"", "nonroot-static-v1"} {
		for _, scenario := range []struct {
			manager        string
			apply, coowned bool
		}{
			{manager: "kubectl-patch"},
			{manager: "kubectl-client-side-apply"},
			{manager: "kubectl-client-side-apply", apply: true},
			{manager: "kubectl-patch", coowned: true},
			{manager: "helm"},
		} {
			t.Run(fmt.Sprintf("%s/%s/apply=%t/coowned=%t", profile, scenario.manager, scenario.apply, scenario.coowned), func(t *testing.T) {
				live, spec := fixtureRole(t, profile)
				fm := managedfieldstest.NewTestFieldManager(managedfields.NewDeducedTypeConverter(), schema.GroupVersionKind{Group: "rbac.authorization.k8s.io", Version: "v1", Kind: "Role"})
				live.ManagedFields = nil
				baseline := live.DeepCopy()
				baseline.Rules = baseline.Rules[1:]
				if err := fm.Apply(asUnstructured(t, baseline), "helm", false); err != nil {
					t.Fatal(err)
				}
				live.Labels["legacy-owned"] = "keep"
				if scenario.apply {
					if err := fm.Apply(asUnstructured(t, live), scenario.manager, true); err != nil {
						t.Fatal(err)
					}
				} else if err := fm.Update(asUnstructured(t, live), scenario.manager); err != nil {
					t.Fatal(err)
				}
				if scenario.coowned {
					if err := fm.Apply(asUnstructured(t, live), "helm", false); err != nil {
						t.Fatal(err)
					}
				}
				// An unrelated manager's ownership must survive migration, too.
				foreign := &unstructured.Unstructured{Object: map[string]any{"apiVersion": live.APIVersion, "kind": "Role", "metadata": map[string]any{"name": live.Name, "namespace": live.Namespace, "labels": map[string]any{"foreign": "keep"}}}}
				if err := fm.Apply(foreign, "other-controller", false); err != nil {
					t.Fatal(err)
				}
				live = trackedRole(t, fm)
				future := live.DeepCopy()
				future.ManagedFields = nil
				// A synthetic later tightening demonstrates migration really frees
				// rules. This is NOT shipped: update/list have active runtime callers.
				future.Rules = future.Rules[1:]
				if err := fm.Apply(asUnstructured(t, future), "helm", false); err == nil {
					t.Fatal("fixture did not reproduce the pre-migration conflict")
				}
				p, err := migrationPatch(live, spec)
				if err != nil {
					t.Fatal(err)
				}
				migrated := patchRole(t, live, p)
				if err := fm.Update(asUnstructured(t, migrated), migrationManager); err != nil {
					t.Fatal(err)
				}
				got := trackedRole(t, fm)
				if !sameOwnership(migrated.ManagedFields, got.ManagedFields) {
					t.Fatal("server field tracking changed expected ownership")
				}
				if !reflect.DeepEqual(got.Rules, live.Rules) || !reflect.DeepEqual(got.Labels, live.Labels) || !reflect.DeepEqual(got.Annotations, live.Annotations) {
					t.Fatal("migration changed live role content")
				}
				if p, err := migrationPatch(got, spec); err != nil || len(p) != 0 {
					t.Fatalf("repeat: %s %v", p, err)
				}
				for _, entry := range live.ManagedFields {
					if entry.Manager == "other-controller" {
						found := false
						for _, after := range got.ManagedFields {
							if reflect.DeepEqual(entry, after) {
								found = true
							}
						}
						if !found {
							t.Fatal("foreign ownership lost")
						}
					}
				}
				if err := fm.Apply(asUnstructured(t, future), "helm", false); err != nil {
					t.Fatalf("upgrade still conflicts: %v", err)
				}
				if err := fm.Apply(asUnstructured(t, future), "helm", false); err != nil {
					t.Fatalf("repeat upgrade: %v", err)
				}
			})
		}
	}
}

func TestMigrateAPIFailureDoesNotRetryOrSucceed(t *testing.T) {
	r, spec := fixtureRole(t, "")
	client := fake.NewSimpleClientset(r)
	patches := 0
	client.PrependReactor("patch", "roles", func(a ktesting.Action) (bool, runtime.Object, error) {
		patches++
		return true, nil, errors.New("resourceVersion test failed")
	})
	if _, err := migrate(context.Background(), client.RbacV1().Roles(r.Namespace), spec, false); err == nil || patches != 1 {
		t.Fatalf("patches=%d err=%v", patches, err)
	}
}

func TestMigrateRejectsUnexpectedServerResponse(t *testing.T) {
	for _, changed := range []string{"rules", "uid", "labels", "finalizers", "ownership", "not applied"} {
		t.Run(changed, func(t *testing.T) {
			r, spec := fixtureRole(t, "")
			client := fake.NewSimpleClientset(r)
			client.PrependReactor("patch", "roles", func(a ktesting.Action) (bool, runtime.Object, error) {
				after := patchRole(t, r, a.(ktesting.PatchAction).GetPatch())
				switch changed {
				case "rules":
					after.Rules = after.Rules[1:]
				case "uid":
					after.UID = "different"
				case "labels":
					after.Labels["keep"] = "changed"
				case "finalizers":
					after.Finalizers = []string{"unexpected"}
				case "ownership":
					after.ManagedFields = after.ManagedFields[:1]
				case "not applied":
					after = r.DeepCopy()
				}
				return true, after, nil
			})
			if changed, err := migrate(t.Context(), client.RbacV1().Roles(r.Namespace), spec, false); err == nil || changed {
				t.Fatalf("unexpected response accepted: %t %v", changed, err)
			}
		})
	}
}

func TestMigrateHTTPDryRunThenApplyThenNoop(t *testing.T) {
	live, spec := fixtureRole(t, "")
	initial := live.DeepCopy()
	patches := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != "/apis/rbac.authorization.k8s.io/v1/namespaces/pipelines/roles/oberth-argo" {
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(404)
			return
		}
		switch r.Method {
		case http.MethodGet:
			_ = json.NewEncoder(w).Encode(live)
		case http.MethodPatch:
			patches++
			if r.Header.Get("Content-Type") != "application/json-patch+json" || r.URL.Query().Get("fieldManager") != migrationManager || r.URL.Query().Get("force") != "" {
				t.Error("wrong patch type/manager or forced request")
			}
			b, err := io.ReadAll(r.Body)
			if err != nil {
				t.Error(err)
				w.WriteHeader(400)
				return
			}
			decoded, err := jsonpatch.DecodePatch(b)
			if err != nil {
				t.Error(err)
				w.WriteHeader(400)
				return
			}
			body, _ := json.Marshal(live)
			body, err = decoded.Apply(body)
			if err != nil {
				t.Error(err)
				w.WriteHeader(409)
				return
			}
			var after rbacv1.Role
			if err := json.Unmarshal(body, &after); err != nil {
				t.Error(err)
				w.WriteHeader(400)
				return
			}
			if patches == 1 {
				if r.URL.Query().Get("dryRun") != metav1.DryRunAll {
					t.Error("dry run did not reach API")
				}
			} else {
				if r.URL.Query().Get("dryRun") != "" {
					t.Error("unexpected dry run")
				}
				after.ResourceVersion = "18"
				live = &after
			}
			_ = json.NewEncoder(w).Encode(after)
		default:
			t.Error("unexpected method")
			w.WriteHeader(405)
		}
	}))
	defer server.Close()
	client, err := rbacclient.NewForConfig(&rest.Config{Host: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	for _, dryRun := range []bool{true, false} {
		changed, err := migrate(t.Context(), client.Roles(spec.Namespace), spec, dryRun)
		if err != nil || !changed {
			t.Fatalf("dryRun=%t changed=%t err=%v", dryRun, changed, err)
		}
		if dryRun && !reflect.DeepEqual(live, initial) {
			t.Fatal("dry run persisted")
		}
	}
	if changed, err := migrate(t.Context(), client.Roles(spec.Namespace), spec, false); err != nil || changed || patches != 2 {
		t.Fatalf("no-op: changed=%t patches=%d err=%v", changed, patches, err)
	}
}

func TestMigrationCreatesHelmApplyOwnerAndSupportsConfiguredIdentity(t *testing.T) {
	r, spec := fixtureRole(t, "nonroot-static-v1")
	spec.CredentialedSA = "custom-release"
	for i := range r.Rules {
		if reflect.DeepEqual(r.Rules[i].Resources, []string{"serviceaccounts/token"}) {
			r.Rules[i].ResourceNames = []string{spec.CredentialedSA}
		}
	}
	r.ManagedFields = r.ManagedFields[1:]
	r.ManagedFields[0].FieldsV1.Raw = []byte(`{"f:rules":{}}`)
	p, err := migrationPatch(r, spec)
	if err != nil {
		t.Fatal(err)
	}
	after := patchRole(t, r, p)
	if len(after.ManagedFields) != 1 || after.ManagedFields[0].Manager != "helm" || after.ManagedFields[0].Operation != metav1.ManagedFieldsOperationApply {
		t.Fatalf("unexpected ownership %+v", after.ManagedFields)
	}
	if p, err := migrationPatch(after, spec); err != nil || len(p) != 0 {
		t.Fatalf("repeat %s %v", p, err)
	}
}

func TestMigrationCLIRequiresExplicitTargetBeforeLoadingKubeconfig(t *testing.T) {
	for _, args := range [][]string{
		nil,
		{"--namespace=pipelines", "--role=oberth-argo", "--release=oberth", "--release-namespace=server"},
		{"--context=explicit", "--namespace=pipelines", "--role=oberth-argo", "--release=oberth", "--release-namespace=server", "--timeout=0s"},
		{"--context=explicit", "--namespace=pipelines", "--role=oberth-argo", "--release=oberth", "--release-namespace=server", "--controller-profile=unknown"},
	} {
		args = append(args, "--kubeconfig=/does/not/exist")
		if err := run(t.Context(), args, io.Discard); err == nil || strings.Contains(err.Error(), "does/not/exist") {
			t.Fatalf("arguments did not fail before config loading: %v", err)
		}
	}
}
