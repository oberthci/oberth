package argojob_test

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/argojob/fake"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/pkg/argoworkflow"
	core "k8s.io/api/core/v1"
)

const secretHelperTestImage = "europe-west4-docker.pkg.dev/skipopsmain/oberth/oberth@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

func TestSecretHelperPreparedPayload(t *testing.T) {
	t.Parallel()
	p, err := prepareSelectedSecrets(t, secretSelectionWorkflow("container", true), []admissioncontract.GrantEvidence{selectedGrant()}, argoworkflow.Policy{})
	if err != nil {
		t.Fatal(err)
	}
	native := &fake.Workflows{}
	runs := memoryAdmission(t, p)
	e := argojob.New(argojob.Config{InstanceID: "frontdoor", Workflows: native, Runs: runs, Progress: runs})
	if _, err = e.Create(t.Context(), p); err != nil {
		t.Fatal(err)
	}
	w := native.Object.DeepCopy()
	requireSubmittedInstance(t, w)
	w.UID, w.ResourceVersion = "", ""
	raw, err := json.MarshalIndent(w, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	assertDefinitionPayload(t, "workflow-secret-helper", raw)
}

func TestSecretHelperFormsAndFrozenVersion(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"container", "script", "container-set", "init", "defaults", "inline"} {
		t.Run(kind, func(t *testing.T) { checkSecretHelperForm(t, kind) })
	}
}

func secretHelperWorkflow(kind string) *wf.Workflow {
	w := secretSelectionWorkflow("container", true)
	if kind == "script" || kind == "container-set" {
		w = secretSelectionWorkflow(kind, true)
	}
	leaf := &w.Spec.Templates[1]
	switch kind {
	case "container":
		leaf.Container.Image = "example.test/scratch:1"
		leaf.Container.SecurityContext = &core.SecurityContext{ReadOnlyRootFilesystem: new(true)}
	case "container-set":
		leaf.ContainerSet.Containers = append(leaf.ContainerSet.Containers, wf.ContainerNode{Container: core.Container{Name: "check", Image: "build:1", Command: []string{"true"}}})
	case "init":
		leaf.InitContainers = []wf.UserContainer{{Container: core.Container{Name: "author-init", Image: "build:1", Command: []string{"true"}}, MirrorVolumeMounts: new(true)}}
	case "defaults":
		w.Spec.TemplateDefaults = &wf.Template{Container: &core.Container{Env: []core.EnvVar{{Name: "DEFAULT", Value: "present"}}}, InitContainers: []wf.UserContainer{{Container: core.Container{Name: "author-init", Image: "build:1", Command: []string{"true"}}}}}
	case "inline":
		w.Spec.Templates[0].DAG.Tasks[0] = wf.DAGTask{Name: "private", Inline: leaf.DeepCopy()}
		w.Spec.Templates = append(w.Spec.Templates[:1], w.Spec.Templates[2:]...)
	}
	return w
}

func checkSecretHelperForm(t *testing.T, kind string) {
	t.Helper()
	w := secretHelperWorkflow(kind)
	grant := selectedGrant()
	if kind == "inline" {
		grant.Step = "*"
	}
	first, err := prepareSelectedSecrets(t, w, []admissioncontract.GrantEvidence{grant}, argoworkflow.Policy{})
	if err != nil {
		t.Fatal(err)
	}
	before, _ := json.Marshal(first.View())
	secondImage := strings.ReplaceAll(secretHelperTestImage, "bbbb", "cccc")
	second, err := prepareSelectedSecretImage(t, w, []admissioncontract.GrantEvidence{grant}, secretPreparation{image: secondImage})
	if err != nil {
		t.Fatal(err)
	}
	after, _ := json.Marshal(first.View())
	if !bytes.Equal(before, after) || first.View().SpecDigest == second.View().SpecDigest {
		t.Fatal("upgrade did not preserve the first run and change the next run")
	}
	if !reflect.DeepEqual(first.View().Plan, second.View().Plan) || len(first.View().Plan.Entries) != 2 {
		t.Fatal("helper became a visible step or changed the plan")
	}
	for _, prepared := range []*admissioncontract.PreparedAdmission{first, second} {
		leaf := prepared.View().Workflow.Spec.Templates[1]
		if kind == "inline" {
			leaf = *prepared.View().Workflow.Spec.Templates[0].DAG.Tasks[0].Inline
		}
		checkHelperShape(t, leaf)
		wantImage := secretHelperTestImage
		if prepared == second {
			wantImage = secondImage
		}
		if leaf.InitContainers[0].Image != wantImage {
			t.Fatal("helper did not retain installation image")
		}
	}
}

func checkHelperShape(t *testing.T, leaf wf.Template) {
	t.Helper()
	if len(leaf.InitContainers) == 0 || leaf.InitContainers[0].Name != "oberth-secret-install" {
		t.Fatal("helper must precede authored init containers")
	}
	checkHelperInstaller(t, leaf.InitContainers[0])
	if *leaf.AutomountServiceAccountToken {
		t.Fatal("helper gained ambient identity")
	}
	for _, c := range secretLeafContainers(leaf) {
		mount := c.VolumeMounts[len(c.VolumeMounts)-1]
		if mount.Name != "oberth-secret-bin" || mount.MountPath != "/run/oberth/bin" || !mount.ReadOnly {
			t.Fatal("step cannot read the installed program")
		}
	}
	volume := leaf.Volumes[len(leaf.Volumes)-1]
	if volume.Name != "oberth-secret-bin" || volume.EmptyDir == nil || volume.EmptyDir.Medium != "" {
		t.Fatal("program volume is not a private disk directory")
	}
}

func checkHelperInstaller(t *testing.T, init wf.UserContainer) {
	t.Helper()
	want := wf.UserContainer{Container: core.Container{
		Name: "oberth-secret-install", Image: init.Image, ImagePullPolicy: core.PullIfNotPresent,
		Command:      []string{"/usr/local/bin/oberth", "internal-install", "--to", "/run/oberth/bin/oberth"},
		VolumeMounts: []core.VolumeMount{{Name: "oberth-secret-bin", MountPath: "/run/oberth/bin"}},
		SecurityContext: &core.SecurityContext{
			RunAsUser: new(int64(65532)), RunAsNonRoot: new(true), ReadOnlyRootFilesystem: new(true),
			AllowPrivilegeEscalation: new(false), Capabilities: &core.Capabilities{Drop: []core.Capability{"ALL"}},
			SeccompProfile: &core.SeccompProfile{Type: core.SeccompProfileTypeRuntimeDefault},
		},
	}, MirrorVolumeMounts: new(false)}
	if !reflect.DeepEqual(init, want) {
		t.Fatal("copy container gained configuration, credentials or authored inputs", init)
	}
}

func TestSecretHelperImageFaultRequiresOperator(t *testing.T) {
	t.Parallel()
	for _, image := range []string{"", "europe-west4-docker.pkg.dev/skipopsmain/oberth/oberth:dev", "europe-west4-docker.pkg.dev/skipopsmain/oberth/oberth@sha256:bad"} {
		for _, declared := range []bool{false, true} {
			w := secretSelectionWorkflow("container", declared)
			w.Spec.Templates[2].Container.ReadinessProbe = &core.Probe{ProbeHandler: core.ProbeHandler{Exec: &core.ExecAction{Command: []string{"true"}}}}
			var grants []admissioncontract.GrantEvidence
			if declared {
				grants = []admissioncontract.GrantEvidence{selectedGrant()}
			}
			p, err := prepareSelectedSecretImage(t, w, grants, secretPreparation{image: image})
			if declared {
				checkHelperImageFault(t, p, err)
			}
			// An ungranted step is hold-eligible here, and holds need an image of their own.
			if !declared && image != "" && err != nil {
				t.Fatal("non-declaring step depends on helper image", err)
			}
		}
	}
}

func TestSecretHelperReservedInputsRefused(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, remedy string
		change       func(*wf.Workflow)
	}{
		{"container", "Remove this field", func(w *wf.Workflow) { w.Spec.Templates[1].Container.Name = "oberth-secret-install" }},
		{"init", "Remove this field", func(w *wf.Workflow) {
			w.Spec.Templates[1].InitContainers = []wf.UserContainer{{Container: core.Container{Name: "oberth-secret-install", Image: "other:1", Command: []string{"other"}}}}
		}},
		{"volume", "Remove this field", func(w *wf.Workflow) {
			w.Spec.Templates[1].Volumes = []core.Volume{{Name: "oberth-secret-bin", VolumeSource: core.VolumeSource{EmptyDir: &core.EmptyDirVolumeSource{}}}}
		}},
		{"mount", "Choose a clean path", func(w *wf.Workflow) {
			w.Spec.Templates[1].Container.VolumeMounts = []core.VolumeMount{{Name: "authored", MountPath: "/run/oberth/bin"}}
		}},
		{"mount-parent", "Choose a clean path", func(w *wf.Workflow) {
			w.Spec.Templates[1].Container.VolumeMounts = []core.VolumeMount{{Name: "authored", MountPath: "/var/run/oberth"}}
		}},
		{"workflow-patch", "Remove podSpecPatch", func(w *wf.Workflow) {
			w.Spec.PodSpecPatch = `{"initContainers":[{"name":"oberth-secret-install","image":"other:1"}]}`
		}},
		{"template-patch", "Remove podSpecPatch", func(w *wf.Workflow) {
			w.Spec.Templates[1].PodSpecPatch = `{"initContainers":[{"name":"oberth-secret-install","command":["other"]}]}`
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := secretWorkflow()
			tc.change(w)
			p, err := prepareSelectedSecrets(t, w, []admissioncontract.GrantEvidence{selectedGrant()}, argoworkflow.Policy{})
			if p != nil || err == nil || !strings.Contains(err.Error(), tc.remedy) {
				t.Fatal("reserved helper input was not refused with a remedy", err)
			}
		})
	}
}

func TestSecretHelperWildcardSkipsNonExecutableLeaves(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"resource", "suspend"} {
		t.Run(kind, func(t *testing.T) { checkSecretHelperNonExecutableLeaf(t, kind) })
	}
}

func checkSecretHelperNonExecutableLeaf(t *testing.T, kind string) {
	t.Helper()
	var before []byte
	for _, declared := range []bool{false, true} {
		w, policy := mixedSecretHelperWorkflow(kind, declared)
		grant := selectedGrant()
		grant.Step = "*"
		p, err := prepareSelectedSecrets(t, w, []admissioncontract.GrantEvidence{grant}, policy)
		if err != nil {
			t.Fatal("wildcard grant refused a non-executable step", err)
		}
		gate := p.View().Workflow.Spec.Templates[2]
		raw, err := json.Marshal(gate)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(raw, []byte("oberth-secret-bin")) || bytes.Contains(raw, []byte("oberth-secret-install")) {
			t.Fatal("non-executable leaf gained the program", kind)
		}
		if declared {
			if !bytes.Equal(before, raw) {
				t.Fatal("helper changed non-executable leaf bytes", kind)
			}
			checkHelperShape(t, p.View().Workflow.Spec.Templates[1])
		}
		before = raw
	}
}

func mixedSecretHelperWorkflow(kind string, declared bool) (*wf.Workflow, argoworkflow.Policy) {
	w := secretSelectionWorkflow("container", declared)
	w.Spec.Templates[0].DAG.Tasks[1] = wf.DAGTask{Name: "gate", Template: "gate"}
	w.Spec.Templates[2] = wf.Template{Name: "gate", Suspend: &wf.SuspendTemplate{}}
	policy := argoworkflow.Policy{}
	if kind == "resource" {
		w.Spec.Templates[2] = wf.Template{Name: "gate", Resource: &wf.ResourceTemplate{Action: "get", Manifest: "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: example, namespace: deploy}"}}
		policy = argoworkflow.Policy{ServerNamespace: "oberth", ArgoNamespace: "argo", Capability: &argoworkflow.Capability{Namespace: "deploy", ServiceAccount: "resources", ResourceKinds: []argoworkflow.ResourceKind{{APIVersion: "v1", Kind: "ConfigMap", Actions: []string{"get"}}}}}
	}
	return w, policy
}

func TestSecretHelperLeavesUndeclaredStepBytesUnchanged(t *testing.T) {
	t.Parallel()
	var plain []byte
	for _, declared := range []bool{false, true} {
		var grants []admissioncontract.GrantEvidence
		if declared {
			grants = []admissioncontract.GrantEvidence{selectedGrant()}
		}
		p, err := prepareSelectedSecrets(t, secretSelectionWorkflow("container", declared), grants, argoworkflow.Policy{})
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(p.View().Workflow.Spec.Templates[2])
		if declared && !bytes.Equal(raw, plain) {
			t.Fatal("another step's secret declaration changed plain step bytes")
		}
		plain = raw
	}
}

func TestSecretHelperUsesBoundedDiskForProgram(t *testing.T) {
	t.Parallel()
	p, err := prepareSelectedSecrets(t, secretSelectionWorkflow("container", true), []admissioncontract.GrantEvidence{selectedGrant()}, argoworkflow.Policy{})
	if err != nil {
		t.Fatal(err)
	}
	leaf := p.View().Workflow.Spec.Templates[1]
	checkHelperShape(t, leaf)
	volumes := map[string]core.Volume{}
	for _, volume := range leaf.Volumes {
		volumes[volume.Name] = volume
	}
	program := volumes["oberth-secret-bin"].EmptyDir
	if program == nil {
		t.Fatal("program directory missing")
	}
	if program.Medium != "" || program.SizeLimit == nil {
		t.Fatal("program is not on bounded disk", program)
	}
	if program.SizeLimit.Value() != argojob.SecretHelperVolumeBytes || argojob.SecretHelperVolumeBytes != 256<<20 {
		t.Fatal("program disk limit differs", program.SizeLimit)
	}
	scratch := volumes["oberth-secret-scratch"].EmptyDir
	if scratch == nil || scratch.Medium != core.StorageMediumMemory {
		t.Fatal("secret files left private memory", scratch)
	}
}

func checkHelperImageFault(t *testing.T, p *admissioncontract.PreparedAdmission, err error) {
	t.Helper()
	if p != nil || err == nil {
		t.Fatal("unpinned installation image admitted", err)
	}
	problem := publicerr.Classify(err)
	if problem.Code() != publicerr.InvalidInput || !publicerr.Permanent(err) {
		t.Fatal("installation fault was classed as transient", problem)
	}
	if problem.Reason() != "Installation image is not digest-pinned" || problem.Remedy() != "Ask the operator to install or upgrade Oberth with a digest-pinned release." {
		t.Fatal("installation fault lacks the operator action", problem)
	}
}
