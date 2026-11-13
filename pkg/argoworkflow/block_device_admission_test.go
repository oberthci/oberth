package argoworkflow

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestBlockVolumeClaimTemplateDeviceAdmissionIsUnchanged(t *testing.T) {
	source := strings.Replace(admissibleDocument, "  templates:\n", "  volumeClaimTemplates:\n  - metadata: {name: block}\n    spec:\n      accessModes: [ReadWriteOnce]\n      volumeMode: Block\n      resources:\n        requests: {storage: 1Gi}\n  templates:\n", 1)
	source = strings.Replace(source, "        command: [/bin/true]\n", "        command: [/bin/true]\n        volumeDevices:\n        - name: block\n          devicePath: /dev/work-disk\n", 1)
	workflow, err := Decode([]byte(source))
	if err != nil {
		t.Fatal(err)
	}
	if err = Admit(workflow, Policy{}); err != nil {
		t.Fatal("block-mode own claim device refused", err)
	}
	device := workflow.Spec.Templates[0].Container.VolumeDevices
	if len(device) != 1 || device[0].Name != "block" || device[0].DevicePath != "/dev/work-disk" || workflow.Spec.VolumeClaimTemplates[0].Spec.VolumeMode == nil || *workflow.Spec.VolumeClaimTemplates[0].Spec.VolumeMode != corev1.PersistentVolumeBlock {
		t.Fatal("authored block device changed", device)
	}
}
