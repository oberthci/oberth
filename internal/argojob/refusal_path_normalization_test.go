package argojob

import (
	"strings"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"

	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/pipeline"
)

func TestRefusalServerMountFiltersNormalizeRelativeAliases(t *testing.T) {
	for _, target := range []string{SourceMountPath, VaultCAMountPath, ReleaseTokenMountPath} {
		for _, alias := range []string{strings.TrimPrefix(target, "/"), "../" + strings.TrimPrefix(target, "/"), "/x/.." + target} {
			t.Run(alias, func(t *testing.T) {
				leaf := &wfv1.Template{Container: &corev1.Container{VolumeMounts: []corev1.VolumeMount{{Name: "authored", MountPath: alias}}}}
				injectTemplateVolumeMounts(leaf, []corev1.VolumeMount{{Name: SourceVolumeName, MountPath: target}}, 0)
				for _, mount := range leaf.Container.VolumeMounts {
					if mount.Name == "authored" {
						t.Fatalf("reserved alias survived: %+v", mount)
					}
				}
			})
		}
	}
	leaf := &wfv1.Template{Container: &corev1.Container{Command: []string{"oberth", "secretstore", "exec"}, VolumeMounts: []corev1.VolumeMount{{Name: "authored", MountPath: "var/run/secrets/kubernetes.io/serviceaccount"}}}}
	// S6 removed the former credential injector. Granted delivery now runs
	// only after admission, which must reject this relative credential alias.
	workflow := &wfv1.Workflow{Spec: wfv1.WorkflowSpec{Templates: []wfv1.Template{*leaf}}}
	if err := argoworkflow.Admit(workflow, argoworkflow.Policy{}); err == nil {
		t.Fatal("relative credential alias reached granted delivery")
	}

}

func TestRefusalWIFPathGuardNormalizesRelativeAliases(t *testing.T) {
	for _, target := range []string{"run/oberth-gar", "../run/oberth-gar/token", "/x/../run/oberth-gar"} {
		request := wifTestRequest(t, func(w *wfv1.Workflow) {
			w.Spec.Templates[0].Container.VolumeMounts = []corev1.VolumeMount{{Name: "authored", MountPath: target}}
		})
		workflow, err := argoworkflow.Decode(request.Source)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := authorizeReleaseWIF(workflow, wifTestConfig(), request); err == nil {
			t.Fatalf("WIF path alias admitted: %q", target)
		}
	}
}

func TestRefusalPinnedSourceGuardNormalizesRelativeAliases(t *testing.T) {
	source := pipeline.PinnedSource{Repository: "upstream/example/source", Commit: strings.Repeat("a", 40), Path: "/work/sources/library"}
	for _, target := range []string{"work/sources/library", "../work/sources/library/file", "work/sources"} {
		workflow := &wfv1.Workflow{Spec: wfv1.WorkflowSpec{Templates: []wfv1.Template{{Container: &corev1.Container{VolumeMounts: []corev1.VolumeMount{{Name: "authored", MountPath: target}}}}}}}
		// The declaration reader is independently exercised by the existing pin tests.
		workflow.Annotations = map[string]string{argoworkflow.SourcesAnnotation: `[{"repository":"` + source.Repository + `","commit":"` + source.Commit + `","path":"` + source.Path + `"}]`}
		request := Request{PinnedSources: []PinnedSourceTree{{Source: source, Directory: "/verified"}}}
		if err := validatePinnedSourceDelivery(workflow, request); err == nil {
			t.Fatalf("pin path alias admitted: %q", target)
		}
	}
}
