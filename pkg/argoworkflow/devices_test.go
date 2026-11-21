package argoworkflow

import (
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/model"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

const kvm = "devices.kubevirt.io/kvm"

func kvmCommand(file, step string) string {
	return "oberth access allow --kind device 'forge/org/repo' '" + file + "' '" + step + "' '" + kvm + "'"
}

func deviceResources(form string) corev1.ResourceRequirements {
	one := corev1.ResourceList{kvm: resource.MustParse("1"), corev1.ResourceCPU: resource.MustParse("1")}
	switch form {
	case "limits":
		return corev1.ResourceRequirements{Limits: one}
	case "requests":
		return corev1.ResourceRequirements{Requests: one}
	}
	return corev1.ResourceRequirements{Limits: one, Requests: one.DeepCopy()}
}

// deviceCarrier puts the request into one container of the main step and
// returns that container's admission path.
func deviceCarrier(t *wfv1.Template, carrier string, r corev1.ResourceRequirements) string {
	helper := corev1.Container{Name: "helper", Image: "example.test/image:1", Resources: r}
	switch carrier {
	case "script":
		t.Container = nil
		t.Script = &wfv1.ScriptTemplate{Container: corev1.Container{Image: "example.test/image:1", Command: []string{"sh"}, Resources: r}, Source: "true"}
		return ".script"
	case "init":
		t.InitContainers = []wfv1.UserContainer{{Container: helper}}
		return ".initContainers[0]"
	case "sidecar":
		t.Sidecars = []wfv1.UserContainer{{Container: helper}}
		return ".sidecars[0]"
	case "containerSet":
		t.Container = nil
		t.ContainerSet = &wfv1.ContainerSetTemplate{Containers: []wfv1.ContainerNode{{Container: corev1.Container{Name: "main", Image: "example.test/image:1", Command: []string{"true"}, Resources: r}}}}
		return ".containerSet.containers[0]"
	}
	t.Container.Resources = r
	return ".container"
}

func deviceGrantFor(step string) admissioncontract.GrantEvidence {
	return clusterGrant(step, "device", kvm)
}

func TestUngrantedDeviceRequestIsRefusedWithItsGrantCommand(t *testing.T) {
	t.Parallel()
	for _, carrier := range []string{"container", "script", "init", "sidecar", "containerSet"} {
		for _, form := range []string{"limits", "requests", "both"} {
			t.Run(carrier+"/"+form, func(t *testing.T) {
				t.Parallel()
				w := simpleWorkflow()
				at := deviceCarrier(&w.Spec.Templates[0], carrier, deviceResources(form))
				_, err := Prepare(t.Context(), requestFor(t, w), Policy{}, identityHooks())
				findings := admissionFindings(t, err)
				want := `spec.templates["main"]` + at + ".resources[" + kvm + "]"
				if len(findings) != 1 || findings[0].Path != want || findings[0].Code != "device" || !findings[0].Installation || findings[0].Command != kvmCommand(".oberth/build.yaml", "main") {
					t.Fatal(findings)
				}
				r := requestFor(t, w)
				r.Grants = []admissioncontract.GrantEvidence{deviceGrantFor("main")}
				if _, err := Prepare(t.Context(), r, Policy{}, identityHooks()); err != nil {
					t.Fatal("granted device refused", err)
				}
			})
		}
	}
}

func TestDeviceGrantCoversOnlyItsRepositoryFileStepAndName(t *testing.T) {
	t.Parallel()
	w := simpleWorkflow()
	w.Spec.Templates[0].Container.Resources = deviceResources("limits")
	for _, field := range []string{"repo", "file", "step", "path", "kind", "state", "revision"} {
		t.Run(field, func(t *testing.T) {
			t.Parallel()
			r := requestFor(t, w)
			grant := deviceGrantFor("main")
			wrongGrant(&grant, field)
			r.Grants = []admissioncontract.GrantEvidence{grant}
			_, err := Prepare(t.Context(), r, Policy{}, identityHooks())
			requireCode(t, err, "device")
		})
	}
	t.Run("other kind of grant", func(t *testing.T) {
		t.Parallel()
		r := requestFor(t, w)
		r.Grants = []admissioncontract.GrantEvidence{clusterGrant("main", "configmap", kvm), clusterGrant("main", "secret", kvm)}
		_, err := Prepare(t.Context(), r, Policy{}, identityHooks())
		requireCode(t, err, "device")
	})
}

// The release tier runs its routed file; a build-file grant never names it.
func TestDeviceGrantKeepsReleaseFileApart(t *testing.T) {
	t.Parallel()
	w := simpleWorkflow()
	w.Spec.Templates[0].Container.Resources = deviceResources("both")
	release := requestFor(t, w)
	release.SelectedFile, release.SourceReceipt.File, release.Trigger, release.Ref = ".oberth/release.yaml", ".oberth/release.yaml", model.TriggerTag, "refs/tags/v1.0.0"
	release.Grants = []admissioncontract.GrantEvidence{deviceGrantFor("main")}
	_, err := Prepare(t.Context(), release, Policy{Release: true}, identityHooks())
	findings := admissionFindings(t, err)
	if len(findings) != 1 || findings[0].Code != "device" || findings[0].Command != kvmCommand(".oberth/release.yaml", "main") {
		t.Fatal(findings)
	}
	grant := deviceGrantFor("main")
	grant.File = ".oberth/release.yaml"
	release.Grants = []admissioncontract.GrantEvidence{grant}
	if _, err := Prepare(t.Context(), release, Policy{Release: true}, identityHooks()); err != nil {
		t.Fatal("release grant refused", err)
	}
	ci := requestFor(t, w)
	ci.Grants = release.Grants
	_, err = Prepare(t.Context(), ci, Policy{}, identityHooks())
	requireCode(t, err, "device")
}

func TestStandardResourcesNeedNoGrant(t *testing.T) {
	t.Parallel()
	w := simpleWorkflow()
	standard := corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("1Gi"), corev1.ResourceEphemeralStorage: resource.MustParse("1Gi"), "hugepages-2Mi": resource.MustParse("2Mi")}
	w.Spec.Templates[0].Container.Resources = corev1.ResourceRequirements{Limits: standard, Requests: standard.DeepCopy()}
	if _, err := Prepare(t.Context(), requestFor(t, w), Policy{}, identityHooks()); err != nil {
		t.Fatal("standard resources refused", err)
	}
}

func TestDefaultDeviceRequestNeedsEachStepGrant(t *testing.T) {
	t.Parallel()
	w := simpleWorkflow()
	w.Spec.TemplateDefaults = &wfv1.Template{Container: &corev1.Container{Resources: deviceResources("limits")}}
	_, err := Prepare(t.Context(), requestFor(t, w), Policy{}, identityHooks())
	findings := admissionFindings(t, err)
	if len(findings) != 1 || findings[0].Path != `spec.templates["main"].container.resources[`+kvm+"]" || findings[0].Command != kvmCommand(".oberth/build.yaml", "main") {
		t.Fatal(findings)
	}
	r := requestFor(t, w)
	r.Grants = []admissioncontract.GrantEvidence{deviceGrantFor("main")}
	if _, err := Prepare(t.Context(), r, Policy{}, identityHooks()); err != nil {
		t.Fatal("granted default device refused", err)
	}
}

func TestSharedTemplateDeviceRequestInheritsNoGrant(t *testing.T) {
	t.Parallel()
	w := sharedConsumer("shared", "work")
	shared := sharedObject("shared")
	shared.Spec.Templates[0].Container.Resources = deviceResources("limits")
	r := requestFor(t, w)
	r.Grants = []admissioncontract.GrantEvidence{deviceGrantFor("*"), deviceGrantFor("work")}
	_, err := Prepare(t.Context(), r, Policy{TemplateGetter: getterFor(map[string]*wfv1.WorkflowTemplate{"shared": shared})}, identityHooks())
	requireCode(t, err, "shared_grant")
}
