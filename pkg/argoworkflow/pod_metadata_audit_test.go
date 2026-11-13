package argoworkflow

import (
	"fmt"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Inventory every JSON-visible field of every reachable type, not just names
// containing metadata/patch. A new opaque field or wrapper must also be reviewed.
func TestPodMetadataPinnedTypeGraphIsReviewed(t *testing.T) {
	module, err := os.ReadFile("../../go.mod")
	if err != nil {
		t.Fatal(err)
	}
	pinned := false
	for _, line := range strings.Split(string(module), "\n") {
		words := strings.Fields(line)
		if len(words) >= 2 && words[0] == "github.com/argoproj/argo-workflows/v4" {
			pinned = words[1] == "v4.0.8"
		}
	}
	if !pinned {
		t.Fatal("Argo version changed: review controller metadata/opaque-spec propagation and the field table")
	}

	visited := map[reflect.Type]bool{}
	var fields []string
	var walk func(reflect.Type)
	walk = func(kind reflect.Type) {
		for kind.Kind() == reflect.Pointer || kind.Kind() == reflect.Map || kind.Kind() == reflect.Slice || kind.Kind() == reflect.Array {
			kind = kind.Elem()
		}
		if kind.Kind() != reflect.Struct || visited[kind] {
			return
		}
		visited[kind] = true
		for i := range kind.NumField() {
			f := kind.Field(i)
			if !f.IsExported() || f.Tag.Get("json") == "-" {
				continue
			}
			fields = append(fields, fmt.Sprintf("%s.%s.%s json=%q type=%s", kind.PkgPath(), kind.Name(), f.Name, f.Tag.Get("json"), f.Type))
			walk(f.Type)
		}
	}
	walk(reflect.TypeOf(wfv1.Workflow{}))
	walk(reflect.TypeOf(wfv1.Template{}))
	slices.Sort(fields)
	body := strings.Join(fields, "\n") + "\n"
	const golden = "testdata/pod-metadata-field-inventory.txt"
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if body != string(want) {
		t.Fatal("Argo/Kubernetes type graph changed: review every new metadata, opaque-spec or Pod-patch route and update the admission table before the inventory")
	}
}

type podMetadataRouteCase struct {
	name, disposition, document string
	allow                       bool
}

func podMetadataRouteCases() []podMetadataRouteCase {
	withSpec := func(insert string) string {
		return strings.Replace(admissibleDocument, "  entrypoint: main\n", "  entrypoint: main\n"+insert, 1)
	}
	withTemplate := func(insert string) string {
		return strings.Replace(admissibleDocument, "    - name: main\n", "    - name: main\n"+insert, 1)
	}
	return []podMetadataRouteCase{
		{"workflow Pod annotations", "refused", withSpec("  podMetadata:\n    annotations: {sidecar.istio.io/inject: 'true'}\n"), false},
		{"workflow Pod labels", "label rule applied", withSpec("  podMetadata:\n    labels: {app: store}\n"), true},
		{"artifactGC annotations", "refused", withSpec("  artifactGC:\n    podMetadata:\n      annotations: {sidecar.istio.io/inject: 'true'}\n"), false},
		{"artifactGC labels", "label rule applied", withSpec("  artifactGC:\n    podMetadata:\n      labels: {app: store}\n"), true},
		{"artifactGC patch", "refused", withSpec("  artifactGC:\n    podSpecPatch: '{\"hostNetwork\":true}'\n"), false},
		{"workflow patch", "refused", withSpec("  podSpecPatch: '{\"hostNetwork\":true}'\n"), false},
		{"template annotations", "refused", withTemplate("      metadata:\n        annotations: {sidecar.istio.io/inject: 'true'}\n"), false},
		{"template labels", "label rule applied", withTemplate("      metadata:\n        labels: {app: store}\n"), true},
		{"template patch", "refused", withTemplate("      podSpecPatch: '{\"hostNetwork\":true}'\n"), false},
		{"templateDefaults annotations", "refused", withSpec("  templateDefaults:\n    metadata:\n      annotations: {sidecar.istio.io/inject: 'true'}\n"), false},
		{"templateDefaults labels", "label rule applied", withSpec("  templateDefaults:\n    metadata:\n      labels: {app: store}\n"), true},
		{"templateDefaults patch", "refused", withSpec("  templateDefaults:\n    podSpecPatch: '{\"hostNetwork\":true}'\n"), false},
		{"PodGC selector", "cannot reach a Pod metadata writer", withSpec("  podGC:\n    strategy: OnPodCompletion\n    labelSelector:\n      matchLabels: {app: store}\n"), true},
		{"workflowMetadata labelsFrom", "cannot reach a Pod", withSpec("  workflowMetadata:\n    labelsFrom:\n      app: {expression: '\"store\"'}\n"), true},
		{"workflowMetadata labelsFrom reserved", "refused", withSpec("  workflowMetadata:\n    labelsFrom:\n      oberth.ci/run: {expression: '\"other\"'}\n"), false},
		{"containerSet metadata", "cannot reach a Pod: no such field", withTemplate("      containerSet:\n        containers:\n        - name: unit\n          image: alpine\n          metadata: {labels: {app: store}}\n"), false},
		{"sidecar metadata", "cannot reach a Pod: no such field", withTemplate("      sidecars:\n      - name: side\n        image: alpine\n        metadata: {labels: {app: store}}\n"), false},
		{"resource manifest no capability", "refused", withTemplate("      resource:\n        action: create\n        manifest: 'apiVersion: v1, kind: Pod'\n"), false},
		{"resource manifestFrom", "refused", withTemplate("      resource:\n        action: create\n        manifestFrom:\n          artifact:\n            name: source\n            artifactGC:\n              podMetadata:\n                annotations: {sidecar.istio.io/inject: 'true'}\n"), false},
		{"data artifact source", "refused", withTemplate("      data:\n        source:\n          artifactPaths:\n            name: source\n            artifactGC:\n              podMetadata:\n                annotations: {sidecar.istio.io/inject: 'true'}\n"), false},
		{"memoize", "refused", withTemplate("      memoize:\n        key: key\n        cache:\n          configMap: {name: cache, key: key}\n"), false},
		{"plugin opaque spec", "refused", withTemplate("      plugin:\n        custom: {podSpecPatch: '{\"hostNetwork\":true}'}\n"), false},
		{"HTTP agent", "refused", withTemplate("      http: {url: 'https://example.test'}\n"), false},
	}
}

func TestEveryPodMetadataAndPatchRoute(t *testing.T) {
	for _, c := range podMetadataRouteCases() {
		t.Run(c.name, func(t *testing.T) {
			wf, err := Decode([]byte(c.document))
			if err == nil {
				err = Admit(wf, Policy{})
			}
			if (err == nil) != c.allow {
				t.Fatal(c.disposition, err)
			}
			t.Log(c.disposition)
		})
	}
}

func TestArtifactGCUsesEveryPodLabelReservation(t *testing.T) {
	for _, key := range []string{"oberth.ci/run", "workflows.argoproj.io/workflow", "app.kubernetes.io/name", "app.kubernetes.io/instance", "component", "{{workflow.parameters.key}}"} {
		wf := decodeAdmissible(t, nil)
		wf.Spec.ArtifactGC = &wfv1.WorkflowLevelArtifactGC{ArtifactGC: wfv1.ArtifactGC{PodMetadata: &wfv1.Metadata{Labels: map[string]string{key: "other"}}}}
		if err := Admit(wf, Policy{}); err == nil {
			t.Fatal("artifactGC bypasses label reservation", key)
		}
	}
}

func TestPodMetadataInvocationTable(t *testing.T) {
	for _, site := range []string{"workflow-exit", "workflow-hook", "DAG-inline", "steps-inline", "nested-inline", "task-exit", "task-hook", "step-exit", "step-hook"} {
		for _, probe := range []string{"annotation", "reserved-label", "label", "patch"} {
			t.Run(site+"/"+probe, func(t *testing.T) {
				wf := decodeAdmissible(t, nil)
				leaf := wf.Spec.Templates[0]
				leaf.Name = "leaf"
				switch probe {
				case "annotation":
					leaf.Metadata.Annotations = map[string]string{"sidecar.istio.io/inject": "true"}
				case "reserved-label":
					leaf.Metadata.Labels = map[string]string{"component": "server"}
				case "label":
					leaf.Metadata.Labels = map[string]string{"app": "store"}
				case "patch":
					leaf.PodSpecPatch = `{"hostNetwork":true}`
				}
				hooks := wfv1.LifecycleHooks{"exit": wfv1.LifecycleHook{Template: "leaf"}}
				var main wfv1.Template
				switch site {
				case "workflow-exit":
					wf.Spec.OnExit = "leaf"
					wf.Spec.Templates = append(wf.Spec.Templates, leaf)
				case "workflow-hook":
					wf.Spec.Hooks = hooks
					wf.Spec.Templates = append(wf.Spec.Templates, leaf)
				case "DAG-inline":
					main = wfv1.Template{Name: "main", DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{{Name: "call", Inline: &leaf}}}}
					wf.Spec.Templates = []wfv1.Template{main}
				case "steps-inline":
					main = wfv1.Template{Name: "main", Steps: []wfv1.ParallelSteps{{Steps: []wfv1.WorkflowStep{{Name: "call", Inline: &leaf}}}}}
					wf.Spec.Templates = []wfv1.Template{main}
				case "nested-inline":
					inner := wfv1.Template{Name: "bridge", DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{{Name: "nested", Inline: &leaf}}}}
					main = wfv1.Template{Name: "main", Steps: []wfv1.ParallelSteps{{Steps: []wfv1.WorkflowStep{{Name: "call", Inline: &inner}}}}}
					wf.Spec.Templates = []wfv1.Template{main}
				case "task-exit", "task-hook":
					task := wfv1.DAGTask{Name: "call", Template: "leaf"}
					if site == "task-exit" {
						task.OnExit = "leaf"
					} else {
						task.Hooks = hooks
					}
					wf.Spec.Templates = []wfv1.Template{{Name: "main", DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{task}}}, leaf}
				case "step-exit", "step-hook":
					step := wfv1.WorkflowStep{Name: "call", Template: "leaf"}
					if site == "step-exit" {
						step.OnExit = "leaf"
					} else {
						step.Hooks = hooks
					}
					wf.Spec.Templates = []wfv1.Template{{Name: "main", Steps: []wfv1.ParallelSteps{{Steps: []wfv1.WorkflowStep{step}}}}, leaf}
				}
				err := Admit(wf, Policy{})
				if (err == nil) != (probe == "label") {
					t.Fatal("recursive metadata route", probe, err)
				}
			})
		}
	}
}

func TestGrantedResourceMetadataAndPatchRoutes(t *testing.T) {
	policy := capabilityPolicyForTest(t)
	for _, payload := range []string{
		"template:\n              metadata:\n                annotations: {sidecar.istio.io/inject: 'true'}",
		"template:\n              metadata:\n                labels: {component: server}",
		"template:\n              spec:\n                podSpecPatch: '{}'",
	} {
		wf, err := Decode([]byte(strings.Replace(capabilityResourceDocument, "operatorDefinedPayload: opaque", payload, 1)))
		if err != nil {
			t.Fatal(err)
		}
		if err := Admit(wf, policy); err == nil {
			t.Fatal("operator resource bypasses Pod metadata or patch rule", payload)
		}
	}
	for _, metadata := range []string{"annotations: {sidecar.istio.io/inject: 'true'}", "labels: {component: server}"} {
		document := strings.Replace(capabilityResourceDocument, "namespace: pipelines\n", "namespace: pipelines\n            "+metadata+"\n", 1)
		wf, err := Decode([]byte(document))
		if err != nil {
			t.Fatal(err)
		}
		if err := Admit(wf, policy); err == nil {
			t.Fatal("resource envelope bypasses Pod metadata rule", metadata)
		}
	}
	wf, err := Decode([]byte(strings.Replace(capabilityResourceDocument, "operatorDefinedPayload: opaque", "template:\n              metadata:\n                labels: {app: store}", 1)))
	if err != nil {
		t.Fatal(err)
	}
	if err := Admit(wf, policy); err != nil {
		t.Fatal("ordinary resource Pod label refused", err)
	}
}

// Unknown operator APIs may put metadata under arbitrary parent names. The
// boundary follows annotations/labels maps through both objects and arrays.
func TestGrantedResourceMetadataUnderArbitraryParents(t *testing.T) {
	policy := capabilityPolicyForTest(t)
	for _, kind := range []ResourceKindCapability{
		{APIVersion: "batch/v1", Kind: "Job", Actions: []string{"create"}},
		{APIVersion: "apps/v1", Kind: "Deployment", Actions: []string{"create"}},
		{APIVersion: "operator.example/v1", Kind: "Worker", Actions: []string{"create"}},
	} {
		policy.Capability.ResourceKinds = []ResourceKindCapability{kind}
		for _, parent := range []string{"template", "workerSettings", "replicas"} {
			for _, metadata := range []string{
				`{"annotations":{"sidecar.istio.io/inject":"true"}}`,
				`{"labels":{"component":"server"}}`,
				`{"Annotations":{"example.test/hook":"enabled"}}`,
				`{"podAnnotations":{"sidecar.istio.io/inject":"true"}}`,
				`{"workerLabels":{"component":"server"}}`,
				`{"podAnnotations":{}}`,
				`{"labels":{"app":"worker"}}`,
			} {
				t.Run(kind.Kind+"/"+parent+"/"+metadata, func(t *testing.T) {
					payload := metadata
					switch parent {
					case "template":
						payload = `{"metadata":` + metadata + `}`
					case "replicas":
						payload = `[{"settings":` + metadata + `}]`
					}
					wf := decodeAdmissible(t, nil)
					wf.Spec.Templates[0].Container = nil
					wf.Spec.Templates[0].Resource = &wfv1.ResourceTemplate{Action: "create", Manifest: fmt.Sprintf(`{"apiVersion":%q,"kind":%q,"metadata":{"name":"worker"},"spec":{%q:%s}}`, kind.APIVersion, kind.Kind, parent, payload)}
					err := Admit(wf, policy)
					if (err == nil) != (metadata == `{"labels":{"app":"worker"}}` || metadata == `{"podAnnotations":{}}`) {
						t.Fatalf("metadata admission = %v", err)
					}
				})
			}
		}
	}
}

// Each metadata-bearing edge needs a disposition, not just a golden inventory
// entry. Review by declaring type+field (never a wildcard) and point Pod writers
// at executable cases in the same route table used by the admission test.
type metadataField struct {
	owner reflect.Type
	name  string
}

type metadataClassification struct {
	reason string
	routes []string
}

func podMetadataClassifications() map[metadataField]metadataClassification {
	return map[metadataField]metadataClassification{
		{reflect.TypeOf(wfv1.Workflow{}), "ObjectMeta"}:                          {"Workflow object only; real controller proof: hack/test-workflow-pod-metadata.py", nil},
		{reflect.TypeOf(wfv1.WorkflowSpec{}), "WorkflowMetadata"}:                {"Workflow object only", []string{"workflowMetadata labelsFrom", "workflowMetadata labelsFrom reserved"}},
		{reflect.TypeOf(wfv1.WorkflowSpec{}), "PodMetadata"}:                     {"Pod writer", []string{"workflow Pod annotations", "workflow Pod labels"}},
		{reflect.TypeOf(wfv1.WorkflowSpec{}), "PodSpecPatch"}:                    {"Opaque Pod patch refused", []string{"workflow patch"}},
		{reflect.TypeOf(wfv1.Template{}), "Metadata"}:                            {"Pod writer; defaults and inline templates share this type", []string{"template annotations", "template labels", "templateDefaults annotations", "templateDefaults labels"}},
		{reflect.TypeOf(wfv1.Template{}), "PodSpecPatch"}:                        {"Opaque Pod patch refused", []string{"template patch", "templateDefaults patch"}},
		{reflect.TypeOf(wfv1.Template{}), "Annotations"}:                         {"Argo template annotation, not Pod metadata (controller uses Template.Metadata)", nil},
		{reflect.TypeOf(wfv1.ArtifactGC{}), "PodMetadata"}:                       {"Artifact GC Pod writer; includes artifact-level GC", []string{"artifactGC annotations", "artifactGC labels", "resource manifestFrom", "data artifact source"}},
		{reflect.TypeOf(wfv1.WorkflowLevelArtifactGC{}), "PodSpecPatch"}:         {"Opaque GC Pod patch refused", []string{"artifactGC patch"}},
		{reflect.TypeOf(wfv1.Metadata{}), "Annotations"}:                         {"Pod metadata value; all incoming edges classified separately", []string{"workflow Pod annotations", "template annotations", "artifactGC annotations"}},
		{reflect.TypeOf(wfv1.Metadata{}), "Labels"}:                              {"Pod label keys checked at each writer", []string{"workflow Pod labels", "template labels", "artifactGC labels"}},
		{reflect.TypeOf(wfv1.WorkflowMetadata{}), "Annotations"}:                 {"Workflow-only annotations; real controller proof", nil},
		{reflect.TypeOf(wfv1.WorkflowMetadata{}), "Labels"}:                      {"Workflow-only labels; reserved keys checked by admitWorkflowMetadata", nil},
		{reflect.TypeOf(wfv1.WorkflowMetadata{}), "LabelsFrom"}:                  {"Workflow-only computed label values; reserved keys checked", []string{"workflowMetadata labelsFrom", "workflowMetadata labelsFrom reserved"}},
		{reflect.TypeOf(corev1.PersistentVolumeClaim{}), "ObjectMeta"}:           {"PVC object metadata; cannot write Pod metadata", nil},
		{reflect.TypeOf(corev1.PersistentVolumeClaimTemplate{}), "ObjectMeta"}:   {"Ephemeral PVC metadata; ephemeral volume source itself is refused", nil},
		{reflect.TypeOf(corev1.PodCertificateProjection{}), "UserAnnotations"}:   {"Projected credentials refused by admitVolumes", nil},
		{reflect.TypeOf(metav1.ObjectMeta{}), "Annotations"}:                     {"Object metadata value; incoming edges classified separately", nil},
		{reflect.TypeOf(metav1.ObjectMeta{}), "Labels"}:                          {"Object metadata value; incoming edges classified separately", nil},
		{reflect.TypeOf(metav1.LabelSelector{}), "MatchLabels"}:                  {"Selector reads labels; does not write Pod metadata", []string{"PodGC selector"}},
		{reflect.TypeOf(corev1.PodAffinityTerm{}), "LabelSelector"}:              {"Native affinity selector; reads Pod labels", nil},
		{reflect.TypeOf(corev1.PodAffinityTerm{}), "MatchLabelKeys"}:             {"Native affinity selector keys; reads Pod labels", nil},
		{reflect.TypeOf(corev1.PodAffinityTerm{}), "MismatchLabelKeys"}:          {"Native affinity selector keys; reads Pod labels", nil},
		{reflect.TypeOf(corev1.ClusterTrustBundleProjection{}), "LabelSelector"}: {"Selects trust bundles; projected credentials refused", nil},
		{reflect.TypeOf(wfv1.PodGC{}), "LabelSelector"}:                          {"PodGC selector reads Pod labels", []string{"PodGC selector"}},
		{reflect.TypeOf(wfv1.Prometheus{}), "Labels"}:                            {"Metric labels; does not write Pod metadata", nil},
	}
}

func metadataLikeField(field reflect.StructField) bool {
	kind := field.Type
	for kind.Kind() == reflect.Pointer || kind.Kind() == reflect.Map || kind.Kind() == reflect.Slice || kind.Kind() == reflect.Array {
		kind = kind.Elem()
	}
	if kind == reflect.TypeOf(metav1.ObjectMeta{}) || kind == reflect.TypeOf(wfv1.Metadata{}) || kind == reflect.TypeOf(wfv1.WorkflowMetadata{}) {
		return true
	}
	name := strings.ToLower(field.Name + " " + strings.Split(field.Tag.Get("json"), ",")[0])
	for _, marker := range []string{"metadata", "annotation", "label", "patch"} {
		if strings.Contains(name, marker) {
			return true
		}
	}
	return false
}

func unclassifiedMetadataFields(root reflect.Type, table map[metadataField]metadataClassification) []metadataField {
	visited := map[reflect.Type]bool{}
	var missing []metadataField
	var walk func(reflect.Type)
	walk = func(kind reflect.Type) {
		for kind.Kind() == reflect.Pointer || kind.Kind() == reflect.Map || kind.Kind() == reflect.Slice || kind.Kind() == reflect.Array {
			kind = kind.Elem()
		}
		if kind.Kind() != reflect.Struct || visited[kind] {
			return
		}
		visited[kind] = true
		for i := range kind.NumField() {
			field := kind.Field(i)
			if !field.IsExported() || field.Tag.Get("json") == "-" {
				continue
			}
			key := metadataField{kind, field.Name}
			if metadataLikeField(field) && table[key].reason == "" {
				missing = append(missing, key)
			}
			walk(field.Type)
		}
	}
	walk(root)
	return missing
}

func TestPodMetadataTypeGraphRoutesAreClassified(t *testing.T) {
	table := podMetadataClassifications()
	for _, field := range unclassifiedMetadataFields(reflect.TypeOf(wfv1.Workflow{}), table) {
		t.Errorf("unclassified metadata route: %s.%s; add a disposition and route-table regression", field.owner, field.name)
	}
	routes := map[string]bool{}
	for _, route := range podMetadataRouteCases() {
		routes[route.name] = true
	}
	for field, classification := range table {
		for _, route := range classification.routes {
			if !routes[route] {
				t.Errorf("%s.%s cites absent route-table case %q", field.owner, field.name, route)
			}
		}
	}
}

func TestPodMetadataCoverageDetectsUnreviewedRoutes(t *testing.T) {
	// A copied golden inventory cannot bless a new field. Exercise unnamed
	// ObjectMeta edges as well as metadata-like names and nested collections.
	type future struct {
		Opaque *metav1.ObjectMeta `json:"opaque"`
		Child  []struct {
			InjectedAnnotations map[string]string `json:"injectedAnnotations"`
		} `json:"child"`
		Known wfv1.Template `json:"known"`
	}
	if missing := unclassifiedMetadataFields(reflect.TypeOf(future{}), podMetadataClassifications()); len(missing) != 2 {
		t.Fatalf("unreviewed routes = %v, want both new fields", missing)
	}
	for field := range podMetadataClassifications() {
		table := podMetadataClassifications()
		delete(table, field)
		if missing := unclassifiedMetadataFields(reflect.TypeOf(wfv1.Workflow{}), table); len(missing) != 1 || missing[0] != field {
			t.Errorf("removing classification of %s.%s did not expose exactly that route: %v", field.owner, field.name, missing)
		}
	}
}
