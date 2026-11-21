package argoworkflow

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/sourcecontract"
	"github.com/oberthci/oberth/internal/workflowpolicy"
	"github.com/oberthci/oberth/pkg/argoworkflow/specsize"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"
)

func TestPublicSourceSpecByteBoundary(t *testing.T) {
	t.Parallel()
	for _, n := range []int{admissioncontract.MaxSourceSpecBytes, admissioncontract.MaxSourceSpecBytes + 1} {
		wf := sourceWorkflow()
		wf.Spec.Templates[0].Container.Args = []string{""}
		size, err := specsize.Submitted(wf)
		if err != nil {
			t.Fatal(err)
		}
		wf.Spec.Templates[0].Container.Args[0] = strings.Repeat("a", n-size)
		err = admitFixture(t.Context(), wf, admissionFixture{})
		boundErr := err
		accepted := n == admissioncontract.MaxSourceSpecBytes
		assertBoundary(t, boundErr, accepted)
		if !accepted {
			requireLimit(t, boundErr, "source_size", "512 KiB")
		}
	}
}
func TestPublicFinalSourceSpecByteBoundary(t *testing.T) {
	t.Parallel()
	for _, n := range []int{admissioncontract.MaxSourceSpecBytes, admissioncontract.MaxSourceSpecBytes + 1} {
		r := requestFor(t, sourceWorkflow())
		bindings := []sourcecontract.Binding{{Template: "main", Artifact: "source", Path: "/work/src", Descriptor: sourcecontract.Descriptor{Version: 1, Kind: sourcecontract.Main}, RepositoryID: 1, Revision: r.SourceReceipt.Revision}}
		hooks := sourceHooks(bindings...)
		hooks.TransformNative = func(_ context.Context, wf *wfv1.Workflow, _ admissioncontract.TransformInput) error {
			wf.Spec.Templates[0].Container.Args = []string{""}
			size, err := specsize.Submitted(wf)
			if err != nil {
				return err
			}
			wf.Spec.Templates[0].Container.Args[0] = strings.Repeat("a", n-size)
			return nil
		}
		_, err := prepareFixture(t.Context(), r, submitPolicy(), hooks)
		boundErr := err
		accepted := n == admissioncontract.MaxSourceSpecBytes
		assertBoundary(t, boundErr, accepted)
		if !accepted {
			requireLimit(t, boundErr, "source_size", "512 KiB")
		}
	}
}
func TestPublicPlanEntryBoundary(t *testing.T) {
	t.Parallel()
	for _, n := range []int{admissioncontract.MaxPlanEntries, admissioncontract.MaxPlanEntries + 1} {
		wf := expansionWorkflow("entries")
		wf.Spec.Templates[0].DAG.Tasks[0].WithItems = make([]wfv1.Item, n)
		for i := range n {
			wf.Spec.Templates[0].DAG.Tasks[0].WithItems[i] = wfv1.Item{Value: json.RawMessage(`1`)}
		}
		plan, err := planFixture(t.Context(), wf, admissionFixture{})
		if err != nil {
			t.Fatal(err)
		}
		if n == admissioncontract.MaxPlanEntries {
			if !plan.Exact || len(plan.Entries) != n {
				t.Fatal("at-N expansion was collapsed")
			}
		} else if plan.Exact || len(plan.Entries) != 1 || !plan.Entries[0].Dynamic {
			t.Fatal("N+1 expansion was not collapsed")
		}
	}
}
func TestPublicPlanByteBoundary(t *testing.T) {
	t.Parallel()
	wf := planByteWorkflow()
	_, base := encodedPlan(t, wf)
	remaining := admissioncontract.MaxPlanBytes - base
	wf.Spec.Templates[0].DAG.Tasks[0].WithItems[0].Value = json.RawMessage(`"` + strings.Repeat("a", remaining/1000) + `"`)
	_, size := encodedPlan(t, wf)
	tail := admissioncontract.MaxPlanBytes - size
	for _, extra := range []int{0, 1} {
		wf.Spec.Templates[0].DAG.Tasks[1].WithItems[0].Value = json.RawMessage(`"` + strings.Repeat("b", tail+extra) + `"`)
		plan, size := encodedPlan(t, wf)
		if extra == 0 && (!plan.Exact || size != admissioncontract.MaxPlanBytes) {
			t.Fatal("exact byte boundary collapsed", size)
		}
		if extra == 1 && (plan.Exact || size > admissioncontract.MaxPlanBytes) {
			t.Fatal("oversized plan stayed exact", size)
		}
	}
}
func planByteWorkflow() *wfv1.Workflow {
	wf := simpleWorkflow()
	work := wf.Spec.Templates[0]
	work.Name = "work"
	items := make([]wfv1.Item, 1000)
	for i := range items {
		items[i] = wfv1.Item{Value: json.RawMessage(`0`)}
	}
	wf.Spec.Templates = []wfv1.Template{{Name: "main", DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{{Name: "big", Template: "group", WithItems: []wfv1.Item{{Value: json.RawMessage(`""`)}}}, {Name: "small", Template: "work", WithItems: []wfv1.Item{{Value: json.RawMessage(`""`)}}}}}}, {Name: "group", DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{{Name: "many", Template: "work", WithItems: items}}}}, work}
	return wf
}
func encodedPlan(t *testing.T, wf *wfv1.Workflow) (admissioncontract.Plan, int) {
	t.Helper()
	plan, err := planFixture(t.Context(), wf, admissionFixture{})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	return plan, len(raw)
}
func TestPublicPlannerVisitBoundary(t *testing.T) {
	t.Parallel()
	// One root + 1040 invocations of a 63-node chain + 15 exit nodes = 65536.
	for _, extra := range []int{0, 1} {
		wf := simpleWorkflow()
		items := make([]wfv1.Item, 1040)
		for i := range items {
			items[i] = wfv1.Item{Value: json.RawMessage(`1`)}
		}
		wf.Spec.Templates = []wfv1.Template{{Name: "main", DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{{Name: "loop", Template: "work0", WithItems: items}}}}}
		wf.Spec.Templates = append(wf.Spec.Templates, nativeChain("work", 63)...)
		wf.Spec.Templates = append(wf.Spec.Templates, nativeChain("exit", 15+extra)...)
		wf.Spec.OnExit = "exit0"
		if err := admitFixture(t.Context(), wf, admissionFixture{}); err != nil {
			t.Fatal(err)
		}
		plan, err := planFixture(t.Context(), wf, admissionFixture{})
		if err != nil {
			t.Fatal(err)
		}
		if plan.Exact != (extra == 0) {
			t.Fatalf("visit boundary extra=%d exact=%v", extra, plan.Exact)
		}
	}
}
func nativeChain(prefix string, n int) []wfv1.Template {
	result := make([]wfv1.Template, n)
	for i := range n {
		result[i] = wfv1.Template{Name: fmt.Sprintf("%s%d", prefix, i)}
		if i == n-1 {
			result[i].Container = &corev1.Container{Image: "image"}
			continue
		}
		result[i].DAG = &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{{Name: "next", Template: fmt.Sprintf("%s%d", prefix, i+1)}}}
	}
	return result
}
func TestPublicTemplateDepthBoundary(t *testing.T) {
	t.Parallel()
	for _, n := range []int{admissioncontract.MaxNestingDepth, admissioncontract.MaxNestingDepth + 1} {
		wf := simpleWorkflow()
		wf.Spec.Entrypoint = "chain0"
		wf.Spec.Templates = nativeChain("chain", n)
		boundErr := admitFixture(t.Context(), wf, admissionFixture{})
		accepted := n == admissioncontract.MaxNestingDepth
		assertBoundary(t, boundErr, accepted)
		if !accepted {
			requireLimit(t, boundErr, "depth", "64")
		}
	}
}
func TestPublicDecodeDepthBoundary(t *testing.T) {
	t.Parallel()
	for _, n := range []int{admissioncontract.MaxNestingDepth, admissioncontract.MaxNestingDepth + 1} {
		value := any("leaf")
		for range n - 4 {
			value = map[string]any{"child": value}
		}
		raw, err := json.Marshal(map[string]any{"apiVersion": "argoproj.io/v1alpha1", "kind": "Workflow", "spec": map[string]any{"templates": []any{map[string]any{"name": "main", "plugin": value}}, "entrypoint": "main"}})
		if err != nil {
			t.Fatal(err)
		}
		_, err = Decode(raw)
		boundErr := err
		accepted := n == admissioncontract.MaxNestingDepth
		assertBoundary(t, boundErr, accepted)
		if !accepted {
			requireLimit(t, boundErr, "depth", "64")
		}
	}
}
func TestPublicTokenLifetimeBoundaries(t *testing.T) {
	t.Parallel()
	for _, n := range []int64{599, 600, 3600, 3601} {
		wf, p := tokenWorkflow()
		wf.Spec.Templates[0].Volumes[0].Projected.Sources[0].ServiceAccountToken.ExpirationSeconds = ptr.To(n)
		boundErr := admitFixture(t.Context(), wf, p)
		accepted := n == 600 || n == 3600
		assertBoundary(t, boundErr, accepted)
		if !accepted {
			requireLimit(t, boundErr, "token", "600")
		}
	}
}
func TestPublicScheduleEntryBoundary(t *testing.T) {
	t.Parallel()
	for _, n := range []int{admissioncontract.MaxScheduleEntryBytes, admissioncontract.MaxScheduleEntryBytes + 1} {
		r := requestFor(t, simpleWorkflow())
		r.Trigger = "schedule"
		r.ScheduleEntry = strings.Repeat("a", n)
		_, err := prepareFixture(t.Context(), r, submitPolicy(), identityHooks())
		boundErr := err
		accepted := n == admissioncontract.MaxScheduleEntryBytes
		assertBoundary(t, boundErr, accepted)
		if !accepted {
			requireLimit(t, boundErr, "schedule", "64")
		}
	}
}
func TestPublicCapabilityKindCountBoundary(t *testing.T) {
	t.Parallel()
	for _, n := range []int{admissioncontract.MaxCapabilityKinds, admissioncontract.MaxCapabilityKinds + 1} {
		p := admissionFixture{Policy: Policy{ServerNamespace: "oberth", ArgoNamespace: "argo", Capability: &Capability{Namespace: "resources", ServiceAccount: "resources"}}, PrepareRequest: admissioncontract.PrepareRequest{IdentityRequest: admissioncontract.IdentityRequest{Namespace: "ci"}}}
		for i := range n {
			p.Capability.ResourceKinds = append(p.Capability.ResourceKinds, ResourceKind{APIVersion: "example.test/v1", Kind: fmt.Sprintf("Kind%d", i), Actions: []string{"get"}})
		}
		boundErr := admitFixture(t.Context(), simpleWorkflow(), p)
		accepted := n == admissioncontract.MaxCapabilityKinds
		assertBoundary(t, boundErr, accepted)
		if !accepted {
			requireLimit(t, boundErr, "kind_count", "32")
		}
	}
}
func TestPublicResourceKindLengthBoundary(t *testing.T) {
	t.Parallel()
	for _, n := range []int{63, 64} {
		kind := strings.Repeat("K", n)
		wf, p := resourceWorkflow(`{"apiVersion":"example.test/v1","kind":"` + kind + `","metadata":{"name":"test","namespace":"resources"}}`)
		p.Capability.ResourceKinds[0].Kind = kind
		boundErr := admitFixture(t.Context(), wf, p)
		accepted := n == 63
		assertBoundary(t, boundErr, accepted)
		if !accepted {
			requireLimit(t, boundErr, "capability", "63")
		}
	}
}
func TestPublicResourceManifestByteBoundary(t *testing.T) {
	t.Parallel()
	for _, n := range []int{128 << 10, (128 << 10) + 1} {
		base := `{"apiVersion":"example.test/v1","kind":"Widget","metadata":{"name":"test","namespace":"resources"},"spec":{"value":""}}`
		manifest := strings.Replace(base, `"value":""`, `"value":"`+strings.Repeat("a", n-len(base))+`"`, 1)
		wf, p := resourceWorkflow(manifest)
		boundErr := admitFixture(t.Context(), wf, p)
		accepted := n == 128<<10
		assertBoundary(t, boundErr, accepted)
		if !accepted {
			requireLimit(t, boundErr, "manifest_size", "128 KiB")
		}
	}
}
func TestPublicPinDestinationNameBoundary(t *testing.T) {
	t.Parallel()
	for _, n := range []int{128, 129} {
		wf := simpleWorkflow()
		a := mainSource()
		a.Path = "/work/sources/" + strings.Repeat("a", n)
		a.Plugin.Configuration = `{"version":1,"kind":"pin","repository":"forge/org/repo","commit":"` + strings.Repeat("a", 40) + `"}`
		wf.Spec.Templates[0].Inputs.Artifacts = []wfv1.Artifact{a}
		boundErr := admitFixture(t.Context(), wf, admissionFixture{})
		accepted := n == 128
		assertBoundary(t, boundErr, accepted)
		if !accepted {
			requireLimit(t, boundErr, "source", "128")
		}
	}
}
func TestPublicDefaultsSnapshotByteBoundary(t *testing.T) {
	t.Parallel()
	for _, n := range []int{admissioncontract.MaxDocumentBytes, admissioncontract.MaxDocumentBytes + 1} {
		r := requestFor(t, simpleWorkflow())
		base := `{"spec":{}}`
		raw := json.RawMessage(base + strings.Repeat(" ", n-len(base)))
		canonical, err := sourcecontract.CanonicalJSON(raw)
		if err != nil {
			t.Fatal(err)
		}
		r.Defaults.WorkflowDefaults = raw
		r.Defaults.Digest = sourcecontract.Digest(canonical)
		_, err = prepareFixture(t.Context(), r, submitPolicy(), identityHooks())
		boundErr := err
		accepted := n == admissioncontract.MaxDocumentBytes
		assertBoundary(t, boundErr, accepted)
		if !accepted {
			requireLimit(t, boundErr, "defaults_size", "1 MiB")
		}
	}
}
func TestPublicTemplateSnapshotByteBoundary(t *testing.T) {
	t.Parallel()
	for _, n := range []int{admissioncontract.MaxDocumentBytes, admissioncontract.MaxDocumentBytes + 1} {
		obj := sharedObject("shared")
		base := `{"templates":[{"name":"work","container":{"image":"image"}}]}`
		raw := json.RawMessage(base + strings.Repeat(" ", n-len(base)))
		p := admissionFixture{Policy: Policy{}, PrepareRequest: admissioncontract.PrepareRequest{IdentityRequest: admissioncontract.IdentityRequest{Namespace: "ci"}, Mode: admissioncontract.ModeInspect, Templates: []admissioncontract.TemplateSnapshot{{Name: obj.Name, Namespace: obj.Namespace, Spec: raw}}}}
		boundErr := admitFixture(t.Context(), sharedConsumer("shared", "work"), p)
		accepted := n == admissioncontract.MaxDocumentBytes
		assertBoundary(t, boundErr, accepted)
		if !accepted {
			requireLimit(t, boundErr, "template_size", "1 MiB")
		}
	}
}
func TestPublicTokenProjectionAndMountCardinality(t *testing.T) {
	t.Parallel()
	wf, p := tokenWorkflow()
	if err := admitFixture(t.Context(), wf, p); err != nil {
		t.Fatal(err)
	}
	wf.Spec.Templates[0].Volumes[0].Projected.Sources = append(wf.Spec.Templates[0].Volumes[0].Projected.Sources, corev1.VolumeProjection{DownwardAPI: &corev1.DownwardAPIProjection{}})
	requireCode(t, admitFixture(t.Context(), wf, p), "token")
	wf, p = tokenWorkflow()
	wf.Spec.Templates[0].Container.VolumeMounts = append(wf.Spec.Templates[0].Container.VolumeMounts, wf.Spec.Templates[0].Container.VolumeMounts[0])
	requireCode(t, admitFixture(t.Context(), wf, p), "token")
}
func TestPublicVolumeSourceCardinality(t *testing.T) {
	t.Parallel()
	wf := simpleWorkflow()
	wf.Spec.Volumes = []corev1.Volume{{Name: "own", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}}
	if err := admitFixture(t.Context(), wf, admissionFixture{}); err != nil {
		t.Fatal(err)
	}
	wf.Spec.Volumes[0].NFS = &corev1.NFSVolumeSource{Server: "storage.example.test", Path: "/data"}
	requireCode(t, admitFixture(t.Context(), wf, admissionFixture{}), "document")
}
func assertBoundary(t *testing.T, err error, accepted bool) {
	t.Helper()
	if (err == nil) != accepted {
		t.Fatalf("accepted=%v error=%v", accepted, err)
	}
}

func TestPublicSourceModeBoundary(t *testing.T) {
	t.Parallel()
	wf := sourceWorkflow()
	if err := admitFixture(t.Context(), wf, admissionFixture{}); err != nil {
		t.Fatal(err)
	}
	wf.Spec.Templates[0].Inputs.Artifacts[0].Mode = ptr.To(int32(0756))
	requireCode(t, admitFixture(t.Context(), wf, admissionFixture{}), "source")
}
func TestPublicMaterializedTemplateByteBoundary(t *testing.T) {
	t.Parallel()
	for _, n := range []int{admissioncontract.MaxDocumentBytes, admissioncontract.MaxDocumentBytes + 1} {
		obj := sharedObject("shared")
		obj.Spec.Templates[0].Container.Args = []string{""}
		raw, err := json.Marshal(obj.Spec)
		if err != nil {
			t.Fatal(err)
		}
		obj.Spec.Templates[0].Container.Args[0] = strings.Repeat("a", n-len(raw))
		p := admissionFixture{Policy: Policy{TemplateGetter: getterFor(map[string]*wfv1.WorkflowTemplate{"shared": obj})}, PrepareRequest: admissioncontract.PrepareRequest{IdentityRequest: admissioncontract.IdentityRequest{Namespace: "ci"}}}
		boundErr := admitFixture(t.Context(), sharedConsumer("shared", "work"), p)
		accepted := n == admissioncontract.MaxDocumentBytes
		assertBoundary(t, boundErr, accepted)
		if !accepted {
			requireLimit(t, boundErr, "template_size", "1 MiB")
		}
	}
}
func TestPublicSourceTagInventoryBoundary(t *testing.T) {
	t.Parallel()
	for _, n := range []int{sourcecontract.MaxRefs, sourcecontract.MaxRefs + 1} {
		r := requestFor(t, sourceWorkflow())
		binding := sourcecontract.Binding{Template: "main", Artifact: "source", Path: "/work/src", Descriptor: sourcecontract.Descriptor{Version: 1, Kind: sourcecontract.Main}, RepositoryID: 1, Revision: r.SourceReceipt.Revision}
		for i := range n {
			binding.Export.Tags = append(binding.Export.Tags, sourcecontract.Ref{Name: fmt.Sprintf("refs/tags/tag-%d", i), ObjectSHA: r.ObjectSHA})
		}
		bindings := []sourcecontract.Binding{binding}
		_, err := prepareFixture(t.Context(), r, submitPolicy(), sourceHooks(bindings...))
		assertBoundary(t, err, n == sourcecontract.MaxRefs)
		if n > sourcecontract.MaxRefs {
			requireLimit(t, err, "tag_count", "4096")
		}
	}
}
func TestPublicSelectedFileByteBoundary(t *testing.T) {
	t.Parallel()
	for _, n := range []int{workflowpolicy.MaxFileBytes, workflowpolicy.MaxFileBytes + 1} {
		file := ".oberth/"
		for len(file)+2 < n {
			file += "a/"
		}
		file += strings.Repeat("b", n-len(file))
		r := requestFor(t, simpleWorkflow())
		r.SelectedFile = file
		r.SourceReceipt.File = file
		_, err := prepareFixture(t.Context(), r, submitPolicy(), identityHooks())
		assertBoundary(t, err, n == workflowpolicy.MaxFileBytes)
		if n > workflowpolicy.MaxFileBytes {
			requireLimit(t, err, "file_size", "1024")
		}
	}
}
func TestPublicResourceActionsAreAFiniteSet(t *testing.T) {
	t.Parallel()
	wf, p := resourceWorkflow(`{"apiVersion":"example.test/v1","kind":"Widget","metadata":{"name":"test","namespace":"resources"}}`)
	p.Capability.ResourceKinds[0].Actions = []string{"create", "get", "delete", "apply"}
	if err := admitFixture(t.Context(), wf, p); err != nil {
		t.Fatal(err)
	}
	p.Capability.ResourceKinds[0].Actions = append(p.Capability.ResourceKinds[0].Actions, "patch")
	requireCode(t, admitFixture(t.Context(), wf, p), "capability")
}

func TestPublicGraphConstructionWorkBoundary(t *testing.T) {
	t.Parallel()
	for _, operations := range []int{maxInspectionNodes, maxInspectionNodes + 1} {
		w := simpleWorkflow()
		tasks := make([]wfv1.DAGTask, operations-3)
		for i := range tasks {
			tasks[i] = wfv1.DAGTask{Name: fmt.Sprintf("call-%d", i), Template: "leaf"}
		}
		w.Spec.Templates = []wfv1.Template{{Name: "main", DAG: &wfv1.DAGTemplate{Tasks: tasks}}, {Name: "leaf", Suspend: &wfv1.SuspendTemplate{}}}
		// Two authored templates, one entrypoint, then one operation per task.
		err := (Policy{}).admit(t.Context(), w, admissioncontract.PrepareRequest{}, identityHooks())
		assertBoundary(t, err, operations == maxInspectionNodes)
		if operations > maxInspectionNodes {
			requireCode(t, err, "inspection")
		}
	}
}

func TestPublicDynamicCandidateConstructionBoundary(t *testing.T) {
	t.Parallel()
	for _, extra := range []int{0, 1} {
		w := simpleWorkflow()
		tasks := make([]wfv1.DAGTask, 1023)
		for i := range tasks {
			tasks[i] = wfv1.DAGTask{Name: fmt.Sprintf("choose-%d", i), Template: "{{inputs.parameters.target}}"}
		}
		if extra == 1 {
			tasks = append(tasks, wfv1.DAGTask{Name: "one-more", Template: "leaf-0"})
		}
		w.Spec.Templates = []wfv1.Template{{Name: "main", DAG: &wfv1.DAGTemplate{Tasks: tasks}}}
		for i := range 62 {
			w.Spec.Templates = append(w.Spec.Templates, wfv1.Template{Name: fmt.Sprintf("leaf-%d", i), Suspend: &wfv1.SuspendTemplate{}})
		}
		// 63 declarations + entrypoint + 1023 calls * (1 call + 63 candidates).
		err := (Policy{}).admit(t.Context(), w, admissioncontract.PrepareRequest{}, identityHooks())
		assertBoundary(t, err, extra == 0)
		if extra == 1 {
			requireCode(t, err, "inspection")
		}
	}
}

func requireLimit(t *testing.T, err error, code, limit string) {
	t.Helper()
	for _, f := range admissionFindings(t, err) {
		if f.Code == code && strings.Contains(f.Reason+" "+f.Remedy, limit) {
			return
		}
	}
	t.Fatalf("refusal must name %s limit %s: %v", code, limit, err)
}
