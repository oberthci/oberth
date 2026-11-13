package argojob

import (
	"encoding/json"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/argoproj/argo-workflows/v4/util/logging"
	argotemplate "github.com/argoproj/argo-workflows/v4/util/template"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/oberthci/oberth/internal/holdfs"
	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/pipeline"
)

func TestBuildParentMountsLeaveHelperLast(t *testing.T) {
	type proof struct {
		Parent   string
		Workflow *wfv1.Workflow
	}
	var proofs []proof
	for _, parent := range []string{"/run", "/var/run", "/var", "/", "/run-tmpfs"} {
		t.Run(parent, func(t *testing.T) {
			path := parent
			if parent == "/run-tmpfs" {
				path = "/run"
			}
			source, err := argoworkflow.Decode([]byte(assistDocument))
			if err != nil {
				t.Fatal(err)
			}
			volume := corev1.Volume{Name: "parent", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: resource.NewQuantity(1<<20, resource.BinarySI)}}}
			if parent == "/run-tmpfs" {
				volume.EmptyDir.Medium = corev1.StorageMediumMemory
			}
			source.Spec.Volumes = append(source.Spec.Volumes, volume)
			leaf := templateNamed(t, source, "test")
			leaf.Container.VolumeMounts = append(leaf.Container.VolumeMounts, corev1.VolumeMount{Name: "parent", MountPath: path})
			raw, err := json.Marshal(source)
			if err != nil {
				t.Fatal(err)
			}
			wf, err := Build(assistConfig(), assistRequest(pipeline.TriggerCI, string(raw)))
			if err != nil {
				t.Fatal("parent mount refused", err)
			}
			compiled := templateNamed(t, wf, "test")
			parentIndex, helperIndex := -1, -1
			for i, m := range compiled.Container.VolumeMounts {
				if m.Name == "parent" && m.MountPath == path {
					parentIndex = i
				}
				if m.MountPath == holdfs.HelperMountPath && m.Name == SourceVolumeName && m.ReadOnly && m.SubPath == "bin" {
					helperIndex = i
				}
			}
			if parentIndex < 0 || helperIndex <= parentIndex || !hasAssistBinary(compiled.Container) {
				t.Fatal("helper not mounted after parent", compiled.Container.VolumeMounts)
			}
			// Resolve both retry branches using pinned Argo's template engine, so the
			// companion proof can run its actual Pod builder including the server patch.
			for _, retry := range []string{"0", "1"} {
				copy := wf.DeepCopy()
				template := templateNamed(t, copy, "test")
				raw, err := json.Marshal(template)
				if err != nil {
					t.Fatal(err)
				}
				resolved, err := argotemplate.Replace(logging.TestContext(t.Context()), string(raw), map[string]any{"retries": retry, "lastRetry.exitCode": "1", "lastRetry.status": "Failed", "lastRetry.duration": "1", "lastRetry.message": "failed"}, true)
				if err != nil {
					t.Fatal(err)
				}
				if err = json.Unmarshal([]byte(resolved), template); err != nil {
					t.Fatal(err)
				}
				proofs = append(proofs, proof{Parent: path, Workflow: copy})
			}
		})
	}
	raw, err := json.Marshal(proofs)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("HELPER_MOUNT_PROOF=%s", raw)
}
