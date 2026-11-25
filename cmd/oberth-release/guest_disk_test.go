package main

import (
	"slices"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/pkg/argoworkflow"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"
)

const runClaim = "{{workflow.name}}"

type guestObject struct {
	APIVersion string `json:"apiVersion"`
	Kind       string
	Metadata   struct {
		Name, Namespace string
		Labels          map[string]string
	}
	Spec struct {
		BackoffLimit *int `json:"backoffLimit"`
		Volumes      []struct {
			Name      string
			Ephemeral struct {
				PersistentVolumeClaim struct{ ClaimName string } `json:"persistentVolumeClaim"`
			}
		}
		Template struct {
			Metadata struct{ Labels map[string]string }
			Spec     struct {
				Containers []struct {
					Command []string
					Env     []corev1.EnvVar
				}
				Volumes []struct {
					PersistentVolumeClaim struct{ ClaimName string } `json:"persistentVolumeClaim"`
				}
			}
		}
	}
}

func guestManifest(t *testing.T, w *wfv1.Workflow, template string) guestObject {
	t.Helper()
	var object guestObject
	if err := yaml.Unmarshal([]byte(w.GetTemplateByName(template).Resource.Manifest), &object); err != nil {
		t.Fatal(template, err)
	}
	return object
}

// The VMI boots only the run's claim, after the Job verified the reviewed pin into it.
func TestWalkBootsOnlyTheVerifiedGuestPin(t *testing.T) {
	w, err := argoworkflow.Decode(ownWorkflow(t, "walk"))
	if err != nil {
		t.Fatal(err)
	}
	assertGuestTasks(t, w)
	assertVerifiedBeforeBoot(t, w)
	pin := w.GetTemplateByName("guest-pin").Outputs.Parameters
	if len(pin) != 2 || pin[0].ValueFrom.Path != "/work/src/.oberth/pins/guest-disk.json" || pin[1].ValueFrom.Path != "/work/src/hack/vm-image/prepare.py" {
		t.Fatal("disk Job does not receive the repository pin and script", pin)
	}
	assertGuestObjects(t, guestManifest(t, w, "guest-claim"), guestManifest(t, w, "guest-disk"), guestManifest(t, w, "vm"))
	assertGuestCleanup(t, w)
}

func assertGuestObjects(t *testing.T, claim, job, vm guestObject) {
	t.Helper()
	for kind, o := range map[string]guestObject{"PersistentVolumeClaim": claim, "Job": job, "VirtualMachineInstance": vm} {
		if o.Kind != kind || o.Metadata.Name != runClaim || o.Metadata.Namespace != "oberth-walk" {
			t.Fatal("guest disk object is not the run's own in the walk namespace", kind, o.Metadata)
		}
	}
	assertGuestJob(t, job)
	if job.Spec.Template.Metadata.Labels[walkRoleLabel] != "guest-disk" || vm.Metadata.Labels[walkRoleLabel] != "walk-vm" {
		t.Fatal("walk Pods escape their NetworkPolicies")
	}
	if volumes := job.Spec.Template.Spec.Volumes; len(volumes) != 1 || volumes[0].PersistentVolumeClaim.ClaimName != runClaim {
		t.Fatal("disk Job writes another claim")
	}
	if root := vm.Spec.Volumes[0]; root.Name != "root" || root.Ephemeral.PersistentVolumeClaim.ClaimName != runClaim {
		t.Fatal("VMI boots a disk the run did not verify", root)
	}
}

// The disk Job's exact contract: a fail-open command, a literal input or a
// retry would let the VMI boot a disk the pin did not verify.
func assertGuestJob(t *testing.T, job guestObject) {
	t.Helper()
	command := []string{"bash", "-ec", `base64 -d <<<"$GUEST_PIN" >/tmp/pin.json; base64 -d <<<"$GUEST_PREPARE" >/tmp/prepare.py; python3 -I /tmp/prepare.py --pin /tmp/pin.json --output /disk/disk.img`}
	env := []corev1.EnvVar{
		{Name: "GUEST_PIN", Value: "{{=sprig.b64enc(inputs.parameters['pin'])}}"},
		{Name: "GUEST_PREPARE", Value: "{{=sprig.b64enc(inputs.parameters['prepare'])}}"},
	}
	containers := job.Spec.Template.Spec.Containers
	if len(containers) != 1 || !slices.Equal(containers[0].Command, command) || !slices.Equal(containers[0].Env, env) {
		t.Fatal("disk Job skips the pinned verification")
	}
	if job.Spec.BackoffLimit == nil || *job.Spec.BackoffLimit != 0 {
		t.Fatal("disk Job retries a failed verification")
	}
}

// Boot waits for a finished, successful verification of the repository pin.
func assertGuestTasks(t *testing.T, w *wfv1.Workflow) {
	t.Helper()
	depends := map[string]string{
		"guest-disk-verified": "guest-disk-pin-read && guest-disk-claimed",
		"walk-vm-boots":       "walk-vm-key-generated && guest-disk-verified",
	}
	arguments := []string{}
	for _, task := range w.GetTemplateByName("vm-walk").DAG.Tasks {
		if want, listed := depends[task.Name]; listed && task.Depends != want {
			t.Fatal("VMI may boot before its pin is verified", task.Name, task.Depends)
		}
		if task.Name == "guest-disk-verified" {
			for _, p := range task.Arguments.Parameters {
				arguments = append(arguments, p.Name+"="+p.Value.String())
			}
		}
	}
	if !slices.Equal(arguments, []string{"pin={{tasks.guest-disk-pin-read.outputs.parameters.pin}}", "prepare={{tasks.guest-disk-pin-read.outputs.parameters.prepare}}"}) {
		t.Fatal("disk Job does not verify the repository pin", arguments)
	}
	r := w.GetTemplateByName("guest-disk").Resource
	if r.Action != "create" || r.SuccessCondition != "status.succeeded > 0" || r.FailureCondition != "status.failed > 0" {
		t.Fatal("boot does not wait for the disk Job to finish", r.SuccessCondition, r.FailureCondition)
	}
}

func assertVerifiedBeforeBoot(t *testing.T, w *wfv1.Workflow) {
	t.Helper()
	p, err := prepareOwn(t, "walk", model.TriggerBranch)
	if err != nil {
		t.Fatal(err)
	}
	order := []string{}
	for _, e := range p.View().Plan.Entries {
		order = append(order, e.Name)
	}
	if slices.Index(order, "guest-disk-verified") > slices.Index(order, "walk-vm-boots") {
		t.Fatal("plan lists boot before verification", order)
	}
}

func assertGuestCleanup(t *testing.T, w *wfv1.Workflow) {
	t.Helper()
	// One group: a failed delete must not skip the others and leak the claim.
	groups := w.GetTemplateByName("cleanup").Steps
	if len(groups) != 1 || len(groups[0].Steps) != 3 {
		t.Fatal("a failed walk delete can skip the others", len(groups))
	}
	deleted := map[string]bool{}
	for _, step := range groups[0].Steps {
		o := guestManifest(t, w, step.Template)
		if w.GetTemplateByName(step.Template).Resource.Action == "delete" && o.Metadata.Name == runClaim {
			deleted[o.Kind] = true
		}
	}
	if len(deleted) != 3 {
		t.Fatal("exit handler leaves a walk object", deleted)
	}
}
