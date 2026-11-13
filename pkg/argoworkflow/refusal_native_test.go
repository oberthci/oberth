package argoworkflow

import (
	"fmt"
	"strings"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
)

func TestRefusalNativeFields(t *testing.T) {
	for _, test := range nativeRefusalCases() {
		t.Run("row-"+test.row, func(t *testing.T) {
			workflow, err := Decode([]byte(admissibleDocument))
			if err != nil {
				t.Fatal(err)
			}
			test.mutate(workflow)
			if err := Admit(workflow, Policy{}); err != nil {
				t.Fatalf("native field refused: %v", err)
			}
		})
	}
}

type nativeRefusalCase struct {
	row    string
	mutate func(*wfv1.Workflow)
}

func nativeRefusalCases() []nativeRefusalCase {
	return []nativeRefusalCase{
		{"9", func(w *wfv1.Workflow) { w.GenerateName = "authored-" }},
		{"20", func(w *wfv1.Workflow) {
			w.Spec.ImagePullSecrets = []corev1.LocalObjectReference{{Name: "private-registry"}}
		}},
		{"33", func(w *wfv1.Workflow) {
			for i := 0; i < 9; i++ {
				w.Spec.VolumeClaimTemplates = append(w.Spec.VolumeClaimTemplates, corev1.PersistentVolumeClaim{})
				w.Spec.VolumeClaimTemplates[i].Name = fmt.Sprintf("data-%d", i)
			}
		}},
		{"34", func(w *wfv1.Workflow) {
			claim := corev1.PersistentVolumeClaim{}
			claim.Name = "data"
			claim.Spec.Resources.Requests = corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("128Gi")}
			w.Spec.VolumeClaimTemplates = []corev1.PersistentVolumeClaim{claim}
		}},
		{"38", func(w *wfv1.Workflow) { w.Spec.ActiveDeadlineSeconds = ptr.To(int64(-1)) }},
		{"43-disk", func(w *wfv1.Workflow) {
			w.Spec.Volumes = []corev1.Volume{{Name: "scratch", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}}
		}},
		{"43-memory", func(w *wfv1.Workflow) {
			w.Spec.Volumes = []corev1.Volume{{Name: "scratch", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{Medium: corev1.StorageMediumMemory}}}}
		}},
		{"44-memory", func(w *wfv1.Workflow) {
			w.Spec.Volumes = []corev1.Volume{{Name: "scratch", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{Medium: corev1.StorageMediumMemory, SizeLimit: resource.NewQuantity(32<<30, resource.BinarySI)}}}}
		}},
		{"44-disk", func(w *wfv1.Workflow) {
			w.Spec.Volumes = []corev1.Volume{{Name: "scratch", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: resource.NewQuantity(100<<30, resource.BinarySI)}}}}
		}},
		{"49", func(w *wfv1.Workflow) { w.Spec.Templates[0].Name = "camelCase"; w.Spec.Entrypoint = "camelCase" }},
		{"51", func(w *wfv1.Workflow) {
			leaf := w.Spec.Templates[0]
			for i := 1; i < 129; i++ {
				copy := *leaf.DeepCopy()
				copy.Name = fmt.Sprintf("leaf-%d", i)
				w.Spec.Templates = append(w.Spec.Templates, copy)
			}
		}},
		{"64", func(w *wfv1.Workflow) {
			leaf := w.Spec.Templates[0]
			leaf.Name = "leaf"
			root := wfv1.Template{Name: "main"}
			for i := 0; i < 65; i++ {
				root.Steps = append(root.Steps, wfv1.ParallelSteps{Steps: []wfv1.WorkflowStep{{Name: fmt.Sprintf("call-%d", i), Template: "leaf"}}})
			}
			w.Spec.Templates = []wfv1.Template{root, leaf}
		}},
		{"65", func(w *wfv1.Workflow) {
			leaf := w.Spec.Templates[0]
			leaf.Name = "leaf"
			root := wfv1.Template{Name: "main", DAG: &wfv1.DAGTemplate{}}
			for i := 0; i < 65; i++ {
				root.DAG.Tasks = append(root.DAG.Tasks, wfv1.DAGTask{Name: fmt.Sprintf("call-%d", i), Template: "leaf"})
			}
			w.Spec.Templates = []wfv1.Template{root, leaf}
		}},
		{"66-sequence", func(w *wfv1.Workflow) {
			leaf := *w.Spec.Templates[0].DeepCopy()
			leaf.Name = "leaf"
			w.Spec.Templates[0].Container = nil
			w.Spec.Templates[0].DAG = &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{{Name: "loop", Template: "leaf", WithSequence: &wfv1.Sequence{Count: ptr.To(intstr.FromInt(101))}}}}
			w.Spec.Templates = append(w.Spec.Templates, leaf)
		}},
		{"67", func(w *wfv1.Workflow) {
			c := *w.Spec.Templates[0].Container
			for i := 0; i < 17; i++ {
				copy := c
				copy.Name = fmt.Sprintf("side-%d", i)
				w.Spec.Templates[0].Sidecars = append(w.Spec.Templates[0].Sidecars, wfv1.UserContainer{Container: copy})
			}
		}},
		{"69-tag", func(w *wfv1.Workflow) { w.Spec.Templates[0].Container.Image = "registry.example.test/runner:latest" }},
		{"69-digest", func(w *wfv1.Workflow) {
			w.Spec.Templates[0].Container.Image = "registry.example.test/runner@sha256:invalid"
		}},
		{"70-empty", func(w *wfv1.Workflow) { w.Spec.Templates[0].Container.Image = "" }},
		{"70-format", func(w *wfv1.Workflow) { w.Spec.Templates[0].Container.Image = " not an image " }},
		{"70-length", func(w *wfv1.Workflow) { w.Spec.Templates[0].Container.Image = strings.Repeat("x", 512) }},
	}
}

func TestRefusalNativeWithItems(t *testing.T) {
	items := strings.TrimSuffix(strings.Repeat("0,", 101), ",")
	source := strings.Replace(admissibleDocument, "      container:", "      steps:\n      - - name: loop\n          template: leaf\n          withItems: ["+items+"]\n    - name: leaf\n      container:", 1)
	mustAdmit(t, source, Policy{})
}

func TestRefusalRecursionHasFiniteInventory(t *testing.T) {
	w, err := Decode([]byte(admissibleDocument))
	if err != nil {
		t.Fatal(err)
	}
	w.Spec.Templates[0].Container = nil
	w.Spec.Templates[0].DAG = &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{{Name: "recur", Template: "main"}}}
	if err := Admit(w, Policy{}); err != nil {
		t.Fatal(err)
	}
	steps, err := PlannedSteps(w)
	if err != nil || len(steps) != 0 {
		t.Fatalf("recursive boundary inventory: %v, %v", steps, err)
	}
}

func TestRefusalDuplicateSchemaChecks(t *testing.T) {
	for _, test := range []nativeRefusalCase{
		{"16", func(w *wfv1.Workflow) { w.Spec.Entrypoint = "" }},
		{"50", func(w *wfv1.Workflow) { w.Spec.Templates = nil }},
		{"52", func(w *wfv1.Workflow) { w.Spec.Templates = append(w.Spec.Templates, *w.Spec.Templates[0].DeepCopy()) }},
		{"54", func(w *wfv1.Workflow) { w.Spec.Entrypoint = "unknown" }},
		{"56", func(w *wfv1.Workflow) {
			w.Spec.Templates[0].Container = nil
			w.Spec.Templates[0].DAG = &wfv1.DAGTemplate{}
			w.Spec.Templates[0].ActiveDeadlineSeconds = ptr.To(intstr.FromInt(1))
		}},
		{"68", func(w *wfv1.Workflow) {
			w.Spec.Templates[0].DAG = &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{{Name: "a", Template: "main", Depends: "b"}, {Name: "b", Template: "main", Dependencies: []string{"a"}}}}
		}},
		{"78-negative", func(w *wfv1.Workflow) {
			w.Spec.Templates[0].Container.Resources.Requests = corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("-1")}
		}},
		{"78-over-limit", func(w *wfv1.Workflow) {
			w.Spec.Templates[0].Container.Resources.Requests = corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")}
			w.Spec.Templates[0].Container.Resources.Limits = corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")}
		}},
		{"80", func(w *wfv1.Workflow) {
			w.Spec.Templates[0].Container.VolumeMounts = []corev1.VolumeMount{{Name: "data", MountPath: "relative/path"}}
		}},
	} {
		t.Run("row-"+test.row, func(t *testing.T) {
			w, err := Decode([]byte(admissibleDocument))
			if err != nil {
				t.Fatal(err)
			}
			test.mutate(w)
			if err := Admit(w, Policy{}); err != nil {
				t.Fatal(err)
			}
		})
	}
	if _, err := Decode(nil); err == nil || strings.Contains(err.Error(), "pipeline document is empty") {
		t.Fatalf("row 1 must use the ordinary decode/type envelope: %v", err)
	}
}
