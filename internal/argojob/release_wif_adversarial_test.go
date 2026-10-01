package argojob

import (
	"encoding/json"
	"strings"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"

	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/periapsis"
)

func TestReleaseWIFRejectsAuthoredProvenanceWithoutCapabilities(t *testing.T) {
	for _, trigger := range []periapsis.Trigger{periapsis.TriggerCI, periapsis.TriggerRelease} {
		for _, enabled := range []bool{false, true} {
			c := wifTestConfig()
			if !enabled {
				c.ReleaseWIF = nil
			}
			r := wifTestRequest(t, func(wf *wfv1.Workflow) {
				wf.Spec.Templates[0].Metadata.Annotations = nil
				wf.Annotations["oberth.ci/release-wif-policy-sha256"] = strings.Repeat("a", 64)
			})
			r.Trigger = trigger
			if _, err := Build(c, r); err == nil || !strings.Contains(err.Error(), "provenance is assigned") {
				t.Fatalf("authored provenance accepted: trigger=%s enabled=%t err=%v", trigger, enabled, err)
			}
		}
	}
}

func TestReleaseWIFConfigCanonicalBindings(t *testing.T) {
	foreignNamespace := wifTestConfig()
	foreignNamespace.ReleaseWIF.Namespace = "foreign-pipelines"
	if err := foreignNamespace.Validate(); err == nil {
		t.Fatal("startup accepted foreign WIF namespace")
	}
	for _, repo := range []string{"/github/skipops/oberth", "github/skipops/oberth.git", "github/../oberth", "github/skipops/*", "github/skipops/ob?erth", "github/skipops/ob\\erth", "release/skipops/oberth", "github/skipops/"} {
		c := wifTestConfig().ReleaseWIF
		c.Repositories = map[string]ReleaseWIFRepository{repo: c.Repositories["github/skipops/oberth"]}
		if err := c.Validate(); err == nil {
			t.Errorf("accepted noncanonical repository %q", repo)
		}
	}
	c := wifTestConfig().ReleaseWIF
	frozen := c.Clone()
	c.Repositories["github/skipops/oberth"].Templates["foreign"] = "chart-writer"
	delete(c.Roles, "image-writer")
	if frozen.Repositories["github/skipops/oberth"].Templates["foreign"] != "" || frozen.Roles["image-writer"].Provider == "" {
		t.Fatal("capability clone retained mutable maps")
	}
}

func TestReleaseWIFComposesVaultAndWaitProtection(t *testing.T) {
	c, r := wifControllerInput(t, "vault")
	wf, err := Build(c, r)
	if err != nil {
		t.Fatal(err)
	}
	leaf := &wf.Spec.Templates[0]
	before := credentialWaitControllerShape(*leaf.Container)
	after := applyCredentialWaitPatch(t, before, leaf.PodSpecPatch)
	found := map[string]bool{}
	for _, m := range after.Containers[1].VolumeMounts {
		found[m.Name] = true
	}
	for _, name := range []string{releaseWIFVolume, SecretsVolumeName, ReleaseTokenVolumeName} {
		if !found[name] {
			t.Errorf("main lost %s", name)
		}
	}
	for _, m := range after.Containers[0].VolumeMounts {
		if m.Name == releaseWIFVolume || m.Name == SecretsVolumeName || m.Name == ReleaseTokenVolumeName {
			t.Errorf("wait retained credential mount %s", m.Name)
		}
	}
	if len(after.Containers[0].VolumeMounts) == 0 {
		t.Fatal("wait lost its independent executor token")
	}
	if wf.Annotations["oberth.ci/release-wif-policy-sha256"] == "" {
		t.Fatal("missing public capability provenance")
	}
	beforeDigest := wf.Annotations[identityAnnotation]
	c.ReleaseWIF.Roles["image-reader"] = ReleaseWIFRole{Provider: "projects/456/locations/global/workloadIdentityPools/reader/providers/issuer", ServiceAccount: "reader@another-project.iam.gserviceaccount.com"}
	changed, err := Build(c, r)
	if err != nil {
		t.Fatal(err)
	}
	if beforeDigest == changed.Annotations[identityAnnotation] {
		t.Fatal("immutable submission identity omitted administrator policy")
	}
}

func TestReleaseWIFRejectsInlineAndCredentialAliases(t *testing.T) {
	for _, shape := range []string{"inline", "claim", "helper-alias", "token-subpath", "defaults-role", "shared-sa", "cross-role-pool"} {
		t.Run(shape, func(t *testing.T) {
			c := wifTestConfig()
			r := wifTestRequest(t, func(wf *wfv1.Workflow) {
				leaf := &wf.Spec.Templates[0]
				switch shape {
				case "inline":
					inner := leaf.DeepCopy()
					leaf.Container = nil
					leaf.Metadata.Annotations = nil
					leaf.DAG = &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{{Name: "child", Inline: inner}}}
				case "claim":
					wf.Spec.VolumeClaimTemplates = append(wf.Spec.VolumeClaimTemplates, corev1.PersistentVolumeClaim{})
					wf.Spec.VolumeClaimTemplates[len(wf.Spec.VolumeClaimTemplates)-1].Name = releaseWIFVolume
				case "helper-alias":
					leaf.Sidecars = []wfv1.UserContainer{{Container: corev1.Container{Name: "helper", Image: leaf.Container.Image, Command: []string{"true"}, VolumeMounts: []corev1.VolumeMount{{Name: releaseWIFVolume, MountPath: "/alias"}}}}}
				case "token-subpath":
					leaf.Container.VolumeMounts = []corev1.VolumeMount{{Name: "work", MountPath: releaseWIFPath + "/token"}}
				case "defaults-role":
					wf.Spec.TemplateDefaults = &wfv1.Template{Metadata: wfv1.Metadata{Annotations: map[string]string{argoworkflow.ReleaseWIFRoleAnnotation: "image-writer"}}}
				}
			})
			if shape == "shared-sa" {
				grant := c.ReleaseWIF.Repositories["github/skipops/oberth"]
				grant.ServiceAccountName = c.CredentialedServiceAccount
				c.ReleaseWIF.Repositories["github/skipops/oberth"] = grant
				c.PerRepoIdentities["github/skipops/oberth"] = PerRepoIdentityConfig{ServiceAccountName: grant.ServiceAccountName}
			}
			if shape == "cross-role-pool" {
				role := c.ReleaseWIF.Roles["image-reader"]
				role.Provider = c.ReleaseWIF.Roles["image-writer"].Provider
				c.ReleaseWIF.Roles["image-reader"] = role
			}
			if _, err := Build(c, r); err == nil {
				t.Fatal("unauthorized projection accepted")
			}
		})
	}
}

var wifControllerShapes = []string{"image-writer", "image-reader", "chart-writer", "script", "helpers", "vault", "sibling"}

// Shared by the normal regression and the retained actual-controller generator.
func wifControllerInput(t *testing.T, shape string) (Config, Request) {
	t.Helper()
	c := wifTestConfig()
	role := "image-writer"
	if strings.HasPrefix(shape, "image-") || shape == "chart-writer" {
		role = shape
	}
	c.ReleaseWIF.Repositories["github/skipops/oberth"].Templates["main"] = role
	r := wifTestRequest(t, func(wf *wfv1.Workflow) {
		leaf := &wf.Spec.Templates[0]
		leaf.Metadata.Annotations[argoworkflow.ReleaseWIFRoleAnnotation] = role
		leaf.Metadata.Annotations[argoworkflow.WorkspaceMountsAnnotation] = "none"
		leaf.Metadata.Annotations[argoworkflow.WorkspaceEnvAnnotation] = "none"
		leaf.Container.VolumeMounts = nil
		switch shape {
		case "script":
			leaf.Script = &wfv1.ScriptTemplate{Container: *leaf.Container, Source: "true"}
			leaf.Container = nil
		case "helpers":
			helper := wfv1.UserContainer{Container: corev1.Container{Name: "helper", Image: leaf.Container.Image, Command: []string{"true"}}, MirrorVolumeMounts: ptr.To(true)}
			leaf.Sidecars = []wfv1.UserContainer{helper}
			helper.Name = "prepare"
			leaf.InitContainers = []wfv1.UserContainer{helper}
		case "vault":
			wf.Annotations[argoworkflow.SecretPathsAnnotation] = "oberth/data/release/r2-upload-token"
			leaf.Container.Command = []string{OberthBinPath, "secretstore", "exec", "--dir=/run/oberth-secrets", "--path=oberth/data/release/r2-upload-token", "--", "/bin/true"}
		case "sibling":
			sibling := leaf.DeepCopy()
			sibling.Name = "ordinary"
			delete(sibling.Metadata.Annotations, argoworkflow.ReleaseWIFRoleAnnotation)
			sibling.AutomountServiceAccountToken = ptr.To(true)
			wf.Spec.Templates = append(wf.Spec.Templates, *sibling)
			wf.Spec.Entrypoint = sibling.Name
		}
	})
	r.Ref = "refs/tags/v1.0.0"
	r.SourceVolume = SourceVolume{ClaimName: "wif-proof-source", SubPath: "src"}
	if shape == "vault" {
		r.ApprovedSecrets["oberth/data/release/r2-upload-token"] = true
	}
	return c, r
}

func TestReleaseWIFDistinctRoleProjection(t *testing.T) {
	seen := map[string]bool{}
	for _, role := range []string{"image-writer", "image-reader", "chart-writer"} {
		c, r := wifControllerInput(t, role)
		wf, err := Build(c, r)
		if err != nil {
			t.Fatal(err)
		}
		leaf := &wf.Spec.Templates[0]
		var adc map[string]any
		if err := json.Unmarshal([]byte(leaf.Metadata.Annotations[releaseWIFADCAnnotation]), &adc); err != nil {
			t.Fatal(err)
		}
		audience := adc["audience"].(string)
		if seen[audience] {
			t.Fatal("role audiences overlap")
		}
		seen[audience] = true
		if adc["type"] != "external_account" || adc["token_url"] != "https://sts.googleapis.com/v1/token" || adc["credential_source"].(map[string]any)["file"] != releaseWIFPath+"/token" {
			t.Fatal("unexpected ADC source or exchange endpoint")
		}
		if !strings.Contains(adc["service_account_impersonation_url"].(string), c.ReleaseWIF.Roles[role].ServiceAccount) {
			t.Fatal("wrong role target")
		}
	}
}
