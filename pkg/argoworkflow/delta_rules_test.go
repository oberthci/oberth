package argoworkflow

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/sourcecontract"
	corev1 "k8s.io/api/core/v1"
)

func deltaBoundaryChecks() []boundaryCheck {
	const at = `spec.templates["main"].volumes[v]`
	return []boundaryCheck{
		{"granted Namespace", "cluster_kind", "policy.capability.resourceKinds[Namespace]", resourceDocument, func(d *checkDocument) {
			d.template().Resource.Manifest = "apiVersion: v1\nkind: Namespace\nmetadata: {name: oberth, namespace: resources}"
			d.policy.Capability.ResourceKinds = []ResourceKind{{APIVersion: "v1", Kind: "Namespace", Actions: []string{"create"}}}
		}},
		{"granted ClusterRoleBinding", "cluster_kind", "policy.capability.resourceKinds[ClusterRoleBinding]", resourceDocument, func(d *checkDocument) {
			d.template().Resource.Manifest = "apiVersion: rbac.authorization.k8s.io/v1\nkind: ClusterRoleBinding\nmetadata: {name: own, namespace: resources}\nroleRef: {apiGroup: rbac.authorization.k8s.io, kind: ClusterRole, name: cluster-admin}\nsubjects: [{kind: ServiceAccount, name: pipeline, namespace: resources}]"
			d.policy.Capability.ResourceKinds = []ResourceKind{{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRoleBinding", Actions: []string{"create"}}}
		}},
		{"store grant cannot authorize cluster Secret", "read", at + ".secret", func(d *checkDocument) {
			d.request.Grants = []admissioncontract.GrantEvidence{clusterGrant("main", "secret", "deploy")}
			declareSecretPaths(d.template(), "deploy")
			d.hooks.LoginAudience = "login"
		}, func(d *checkDocument) {
			d.volume(corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "deploy"}})
		}},
		{"pod certificate", "projection", at + ".projected.sources[0].podCertificate", nil, func(d *checkDocument) {
			d.volume(corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{Sources: []corev1.VolumeProjection{{PodCertificate: &corev1.PodCertificateProjection{SignerName: "kubernetes.io/kube-apiserver-client-pod", KeyType: "ED25519", CredentialBundlePath: "certificate"}}}}})
		}},
		{"cluster trust bundle", "projection", at + ".projected.sources[0].clusterTrustBundle", nil, func(d *checkDocument) {
			d.volume(corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{Sources: []corev1.VolumeProjection{{ClusterTrustBundle: &corev1.ClusterTrustBundleProjection{Path: "ca"}}}}})
		}},
		{"ConfigMapProjection selector", "read", at + ".projected.sources[0].configMap", nil, func(d *checkDocument) {
			d.volume(corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{Sources: []corev1.VolumeProjection{{ConfigMap: &corev1.ConfigMapProjection{LocalObjectReference: corev1.LocalObjectReference{Name: "private"}}}}}})
		}},
		{"explicit resource namespace", "resource_namespace", `spec.templates["main"].resource.manifest.metadata.namespace`, resourceDocument, func(d *checkDocument) {
			d.template().Resource.Manifest = strings.ReplaceAll(d.template().Resource.Manifest, ", namespace: resources", "")
		}},
		{"shared leaf uses consumer pull secrets", "read", "spec.imagePullSecrets[0]", func(d *checkDocument) {
			sharedDocument(d)
			d.wf.Spec.ImagePullSecrets = []corev1.LocalObjectReference{{Name: "private"}}
			d.request.Grants = []admissioncontract.GrantEvidence{clusterGrant("*", "image-pull-secret", "private")}
		}, func(d *checkDocument) { d.request.Grants = nil }},
	}
}

func TestEveryGeneratedClusterKindIsRefusedDespiteGrant(t *testing.T) {
	t.Parallel()
	kinds := clusterScopedKinds()
	if len(kinds) < 39 {
		t.Fatal("incomplete API scope generation", kinds)
	}
	for _, entry := range kinds {
		t.Run(entry, func(t *testing.T) {
			group, kind, _ := strings.Cut(entry, "/")
			version := "v1"
			if group != "" {
				version = group + "/v1"
			}
			w, p := resourceWorkflow("apiVersion: " + version + "\nkind: " + kind + "\nmetadata: {name: own, namespace: resources}")
			p.Capability.ResourceKinds = []ResourceKind{{APIVersion: version, Kind: kind, Actions: []string{"create"}}}
			r := requestFor(t, w)
			r.IdentityRequest.ResourceAccount = "resources"
			prepared, err := Prepare(t.Context(), r, p.Policy, identityHooks())
			if prepared != nil {
				t.Fatal("cluster kind sealed")
			}
			requireCode(t, err, "cluster_kind")
			if !strings.Contains(err.Error(), "cluster-scoped kind "+kind+" cannot be changed by a pipeline") {
				t.Fatal(err)
			}
		})
	}
}

func TestGrantKindsNeverAliasAndEachCommandAdmitsSameDocument(t *testing.T) {
	t.Parallel()
	for _, location := range []string{"volume", "configmap", "configmapProjection", "imagePullSecrets"} {
		t.Run(location, func(t *testing.T) {
			w := simpleWorkflow()
			kind := clusterLocation(w, &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "deploy"}, Key: "token"}, location)
			r := requestFor(t, w)
			_, err := Prepare(t.Context(), r, Policy{}, identityHooks())
			requireCode(t, err, "read")
			command := "oberth access allow --kind " + kind + " 'forge/org/repo' '.oberth/build.yaml' 'main' 'deploy'"
			if !strings.Contains(err.Error(), command) {
				t.Fatal("missing exact typed command", err)
			}
			checkGrantKindMatrix(t, r, kind)
			applyClusterGrantCommand(t, &r, command)
			if _, err := Prepare(t.Context(), r, Policy{}, identityHooks()); err != nil {
				t.Fatal("printed command did not admit same push", err)
			}
		})
	}
}

func checkGrantKindMatrix(t *testing.T, r admissioncontract.PrepareRequest, kind string) {
	t.Helper()
	for _, other := range []string{"secret", "cluster-secret", "configmap", "image-pull-secret"} {
		r.Grants = []admissioncontract.GrantEvidence{clusterGrant("main", other, "deploy")}
		prepared, err := Prepare(t.Context(), r, Policy{}, identityHooks())
		if other != kind {
			requireCode(t, err, "read")
			continue
		}
		if err != nil || prepared == nil {
			t.Fatal("exact grant refused", err)
		}
	}
}

func applyClusterGrantCommand(t *testing.T, r *admissioncontract.PrepareRequest, command string) {
	t.Helper()
	parts := strings.Split(command, "'")
	flags := strings.Fields(parts[0])
	if len(flags) != 5 || strings.Join(flags[:4], " ") != "oberth access allow --kind" || len(parts) != 9 {
		t.Fatal("invalid grant command", command)
	}
	r.Grants = []admissioncontract.GrantEvidence{{Kind: flags[4], Repository: parts[1], File: parts[3], Step: parts[5], Path: parts[7], Revision: "operator-approval", State: admissioncontract.StateVerified}}
}

func TestProtectedClusterNamesStayRefusedWithExactGrants(t *testing.T) {
	t.Parallel()
	for _, location := range []string{"volume", "projection", "configmap", "configmapProjection", "imagePullSecrets", "pvc"} {
		for _, name := range []string{"oberth", "oberth-state"} {
			t.Run(location+"/"+name, func(t *testing.T) {
				w := simpleWorkflow()
				p := Policy{}
				kind := ""
				if location == "pvc" {
					w.Spec.Templates[0].Volumes = []corev1.Volume{{Name: "v", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: name}}}}
					p.PVCClaims = map[string]bool{name: true}
				} else {
					kind = clusterLocation(w, &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: name}}, location)
				}
				// The selector helper's volumes use the ordinary name; replace it with the protected target.
				raw := strings.ReplaceAll(string(mustDocument(t, w)), "private", name)
				var err error
				w, err = Decode([]byte(raw))
				if err != nil {
					t.Fatal(err)
				}
				r := requestFor(t, w)
				r.Grants = []admissioncontract.GrantEvidence{clusterGrant("main", kind, name)}
				code := "reserved"
				if location == "pvc" {
					code = "pvc"
				}
				_, err = Prepare(t.Context(), r, p, identityHooks())
				requireCode(t, err, code)
			})
		}
	}
}

func TestProjectedSourceAllowlistIsClosed(t *testing.T) {
	t.Parallel()
	typ := reflect.TypeFor[corev1.VolumeProjection]()
	allowed := map[string]bool{"secret": true, "configMap": true, "downwardAPI": true, "serviceAccountToken": true}
	for i := range typ.NumField() {
		field := typ.Field(i)
		name := strings.Split(field.Tag.Get("json"), ",")[0]
		if allowed[name] {
			continue
		}
		t.Run(name, func(t *testing.T) {
			w := simpleWorkflow()
			source := corev1.VolumeProjection{}
			reflect.ValueOf(&source).Elem().Field(i).Set(reflect.New(field.Type.Elem()))
			w.Spec.Templates[0].Volumes = []corev1.Volume{{Name: "v", VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{Sources: []corev1.VolumeProjection{source}}}}}
			err := (Policy{}).admit(t.Context(), w, requestFor(t, w), identityHooks())
			findings := admissionFindings(t, err)
			if len(findings) != 1 || findings[0].Path != `spec.templates["main"].volumes[v].projected.sources[0].`+name || findings[0].Code != "projection" || findings[0].Reason != "projected source type is not supported" {
				t.Fatal(err)
			}
		})
	}
}

func TestYAMLTypeErrorsNameEveryFullFieldPath(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ field, value, path string }{
		{"args", `["--retries",3]`, "spec.templates[0].container.args[1]"},
		{"args", `"wrong"`, "spec.templates[0].container.args"},
		{"container", `[]`, "spec.templates[0].container"},
		{"activeDeadlineSeconds", `"wrong"`, "spec.activeDeadlineSeconds"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			w := simpleWorkflow()
			raw := wrongTypedDocument(t, tc.field, tc.value)
			r := requestFor(t, w)
			r.Document = raw
			r.SourceReceipt.DocumentDigest = sourcecontract.Digest(raw)
			prepared, err := Prepare(t.Context(), r, Policy{}, identityHooks())
			if prepared != nil {
				t.Fatal("wrong type sealed")
			}
			findings := admissionFindings(t, err)
			if len(findings) != 1 || findings[0].Code != "field_type" || findings[0].Path != tc.path || !strings.Contains(err.Error(), r.SelectedFile) {
				t.Fatal(err)
			}
		})
	}
}

func wrongTypedDocument(t *testing.T, field, value string) []byte {
	t.Helper()
	var object map[string]any
	if err := json.Unmarshal(mustDocument(t, simpleWorkflow()), &object); err != nil {
		t.Fatal(err)
	}
	spec := object["spec"].(map[string]any)
	template := spec["templates"].([]any)[0].(map[string]any)
	target := template["container"].(map[string]any)
	if field == "container" {
		target = template
	}
	if field == "activeDeadlineSeconds" {
		target = spec
	}
	var typed any
	if err := json.Unmarshal([]byte(value), &typed); err != nil {
		t.Fatal(err)
	}
	target[field] = typed
	raw, err := json.Marshal(object)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestClusterGrantsCannotAuthorizeStorePaths(t *testing.T) {
	t.Parallel()
	w, p := tokenWorkflow()
	declareSecretPaths(&w.Spec.Templates[0], "deploy")
	w.Spec.Templates[0].Container.Command = []string{"oberth", "secretstore", "exec"}
	r := requestFor(t, w)
	for _, kind := range []string{"secret", "cluster-secret", "configmap", "image-pull-secret"} {
		t.Run(kind, func(t *testing.T) {
			r.Grants = append([]admissioncontract.GrantEvidence{clusterGrant("main", kind, "deploy")}, p.Grants...)
			_, err := prepareFixture(t.Context(), r, p, sourceHooks(boundEvidence(r, sourcecontract.Descriptor{Version: 1, Kind: sourcecontract.Main})))
			if kind == "secret" {
				if err != nil {
					t.Fatal("store grant refused", err)
				}
				return
			}
			requireCode(t, err, "grant")
			if !strings.Contains(err.Error(), "oberth secret put --repo 'forge/org/repo' --file '.oberth/build.yaml' --step 'main' 'deploy' < <value-file>") {
				t.Fatal(err)
			}
		})
	}
}

func TestEachProjectedEntryHasExactlyOneSource(t *testing.T) {
	t.Parallel()
	for _, count := range []int{0, 1, 2} {
		w := simpleWorkflow()
		projection := corev1.VolumeProjection{}
		if count > 0 {
			projection.DownwardAPI = &corev1.DownwardAPIProjection{}
		}
		if count > 1 {
			projection.ConfigMap = &corev1.ConfigMapProjection{LocalObjectReference: corev1.LocalObjectReference{Name: "public"}}
		}
		w.Spec.Templates[0].Volumes = []corev1.Volume{{Name: "v", VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{Sources: []corev1.VolumeProjection{projection}}}}}
		r := requestFor(t, w)
		r.Grants = []admissioncontract.GrantEvidence{clusterGrant("main", "configmap", "public")}
		_, err := Prepare(t.Context(), r, Policy{}, identityHooks())
		assertBoundary(t, err, count == 1)
		if count != 1 {
			requireCode(t, err, "document")
		}
	}
}

func TestResourceNamespaceSubstitutionUsesAssignedWorkflowNamespace(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		value, granted string
		allowed        bool
	}{
		{"{{workflow.namespace}}", "ci", false},
		{"{{workflow.namespace}}", "application", false},
		{"{{workflow.name}}", "name", false},
		{"{{workflow.uid}}", "uid", false},
	} {
		t.Run(tc.value+"/"+tc.granted, func(t *testing.T) {
			w, p := resourceWorkflow("apiVersion: example.test/v1\nkind: Widget\nmetadata: {name: child, namespace: '" + tc.value + "'}")
			p.Capability.Namespace = tc.granted
			r := requestFor(t, w)
			r.IdentityRequest.ResourceAccount = "resources"
			_, err := Prepare(t.Context(), r, p.Policy, identityHooks())
			assertBoundary(t, err, tc.allowed)
			if !tc.allowed {
				findings := admissionFindings(t, err)
				if tc.granted == "ci" || tc.value == "{{workflow.namespace}}" {
					requireCode(t, err, "protected_namespace")
				} else if len(findings) != 1 || findings[0].Path != `spec.templates["main"].resource.manifest.metadata.namespace` || findings[0].Code != "capability" {
					t.Fatal(err)
				}
			}
		})
	}
}
