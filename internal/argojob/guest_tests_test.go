package argojob

import (
	"strings"
	"testing"

	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/periapsis"
)

func TestGuestTestsAdmission(t *testing.T) {
	document := strings.Replace(kvmTestDocument, "oberth.ci/kvm-templates:", "oberth.ci/guest-test-templates: kernel-test\n    oberth.ci/kvm-templates:", 1)
	for _, tc := range []struct {
		name           string
		grant, enabled bool
		trigger        periapsis.Trigger
		want           string
	}{
		{"granted", true, true, periapsis.TriggerCI, ""},
		{"absent or revoked", false, true, periapsis.TriggerCI, "admin grant"},
		{"disabled", true, false, periapsis.TriggerCI, "vm.kvm.enabled"},
		{"release", true, true, periapsis.TriggerRelease, "credential-free CI"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config := testConfig()
			config.KVMEnabled = tc.enabled
			request := testRequest(tc.trigger, document)
			request.KubeVirtGranted = tc.grant
			wf, err := Build(config, request)
			if tc.want != "" {
				if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("want %q, got %v", tc.want, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			for _, tmpl := range wf.Spec.Templates {
				if tmpl.Container == nil {
					continue
				}
				main := tmpl.Container
				guest := tmpl.Name == "kernel-test"
				found := false
				for _, env := range main.Env {
					if env.Name == "OBERTH_GUEST_TEST" {
						found = env.Value == "kvm-v1"
					}
				}
				if found != guest {
					t.Fatalf("%s guest marker=%v", tmpl.Name, found)
				}
				if guest {
					quantity := main.Resources.Limits[kvmDeviceResource]
					if quantity.String() != "1" {
						t.Fatal("missing KVM allocation")
					}
					sc := main.SecurityContext
					if sc == nil || sc.Capabilities == nil || len(sc.Capabilities.Add) != 0 || len(sc.Capabilities.Drop) != 1 || sc.Capabilities.Drop[0] != "ALL" || sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation || sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem {
						t.Fatalf("unsafe host context: %+v", sc)
					}
				}
			}
			if wf.Annotations[argoworkflow.GuestTestTemplatesAnnotation] != "kernel-test" {
				t.Fatal("guest declaration lost")
			}
		})
	}
}
