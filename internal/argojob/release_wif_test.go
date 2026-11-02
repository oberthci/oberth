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

func wifTestConfig() Config {
	c := testConfig()
	c.PerRepoIdentities = map[string]PerRepoIdentityConfig{"github/skipops/oberth": {ServiceAccountName: "oberth-per-repo"}}
	c.ReleaseWIF = &ReleaseWIFConfig{
		Namespace: c.Namespace,
		Roles: map[string]ReleaseWIFRole{
			"image-writer": {Provider: "projects/123/locations/global/workloadIdentityPools/oberth-release-images/providers/tuxbox", ServiceAccount: "image-writer@example-project.iam.gserviceaccount.com"},
			"image-reader": {Provider: "projects/123/locations/global/workloadIdentityPools/oberth-release-reader/providers/tuxbox", ServiceAccount: "image-reader@example-project.iam.gserviceaccount.com"},
			"chart-writer": {Provider: "projects/123/locations/global/workloadIdentityPools/oberth-release-charts/providers/tuxbox", ServiceAccount: "chart-writer@example-project.iam.gserviceaccount.com"},
		},
		Repositories: map[string]ReleaseWIFRepository{"github/skipops/oberth": {ServiceAccountName: "oberth-per-repo", Templates: map[string]string{"main": "image-writer"}}},
	}
	return c
}

func wifTestRequest(t *testing.T, mutate func(*wfv1.Workflow)) Request {
	t.Helper()
	wf, err := argoworkflow.Decode([]byte(greedyDocument))
	if err != nil {
		t.Fatal(err)
	}
	wf.Spec.Templates[0].Metadata.Annotations = map[string]string{argoworkflow.ReleaseWIFRoleAnnotation: "image-writer"}
	if mutate != nil {
		mutate(wf)
	}
	source, err := json.Marshal(wf)
	if err != nil {
		t.Fatal(err)
	}
	r := testRequest(periapsis.TriggerRelease, string(source))
	r.UpstreamName = "github"
	return r
}

func TestReleaseWIFBindsOnlyApprovedMainCapability(t *testing.T) {
	for _, shape := range []string{"container", "script", "mirroring-helpers"} {
		t.Run(shape, func(t *testing.T) {
			r := wifTestRequest(t, func(wf *wfv1.Workflow) {
				leaf := &wf.Spec.Templates[0]
				if shape == "script" {
					leaf.Script = &wfv1.ScriptTemplate{Container: *leaf.Container, Source: "true"}
					leaf.Container = nil
				}
				if shape == "mirroring-helpers" {
					helper := wfv1.UserContainer{Container: corev1.Container{Name: "helper", Image: templateMainContainer(leaf).Image, Command: []string{"true"}}, MirrorVolumeMounts: ptr.To(true)}
					leaf.Sidecars = []wfv1.UserContainer{helper}
					leaf.InitContainers = []wfv1.UserContainer{helper}
				}
			})
			wf, err := Build(wifTestConfig(), r)
			if err != nil {
				t.Fatal(err)
			}
			leaf := &wf.Spec.Templates[0]
			if leaf.ServiceAccountName != "oberth-per-repo" || !tokenDisabled(leaf) {
				t.Fatal("identity or explicit automount boundary missing")
			}
			main := templateMainContainer(leaf)
			environment := map[string]string{}
			for _, e := range main.Env {
				environment[e.Name] = e.Value
			}
			if environment["OBERTH_RELEASE_WIF"] != "1" || environment["GOOGLE_APPLICATION_CREDENTIALS"] != "/run/oberth-gar/adc.json" {
				t.Fatal("selected main does not match the helper's explicit opt-in contract")
			}
			var found bool
			for _, m := range main.VolumeMounts {
				if m.Name == releaseWIFVolume {
					found = m.ReadOnly && m.MountPath == releaseWIFPath
				}
			}
			if !found {
				t.Fatal("missing readonly main projection")
			}
			for _, group := range [][]wfv1.UserContainer{leaf.Sidecars, leaf.InitContainers} {
				for _, c := range group {
					if c.MirrorVolumeMounts != nil && *c.MirrorVolumeMounts {
						t.Fatal("helper retains late credential mirror instruction")
					}
					for _, m := range c.VolumeMounts {
						if m.Name == releaseWIFVolume {
							t.Fatal("helper received JWT")
						}
					}
					for _, e := range c.Env {
						if e.Name == "GOOGLE_APPLICATION_CREDENTIALS" || e.Name == "OBERTH_RELEASE_WIF" {
							t.Fatal("helper received ADC")
						}
					}
				}
			}
			projection := leaf.Volumes[len(leaf.Volumes)-1].Projected
			jwt := projection.Sources[0].ServiceAccountToken
			role := wifTestConfig().ReleaseWIF.Roles["image-writer"]
			if jwt.Audience != "https://iam.googleapis.com/"+role.Provider || *jwt.ExpirationSeconds != 600 || jwt.Path != "token" {
				t.Fatalf("wrong token projection: %+v", jwt)
			}
			var adc map[string]any
			if err := json.Unmarshal([]byte(leaf.Metadata.Annotations[releaseWIFADCAnnotation]), &adc); err != nil {
				t.Fatal(err)
			}
			if adc["audience"] != "//iam.googleapis.com/"+role.Provider || !strings.Contains(adc["service_account_impersonation_url"].(string), role.ServiceAccount) {
				t.Fatalf("wrong ADC: %+v", adc)
			}
			if !strings.Contains(leaf.PodSpecPatch, `"$patch":"delete"`) || !strings.Contains(leaf.PodSpecPatch, "/mainctrfs/run/oberth-gar") {
				t.Fatal("executor credential mirror not removed")
			}
		})
	}
}

func TestReleaseWIFRejectsUnapprovedAuthority(t *testing.T) {
	for _, variant := range []string{"disabled", "ci", "repo", "upstream", "namespace", "ci-identity-alias", "repo-identity-alias", "identity-missing", "identity-changed", "role", "leaf", "workflow-optout", "leaf-optout", "defaults", "dag", "root-annotation", "alias", "volume", "common-pool", "malicious-provider", "malicious-account"} {
		t.Run(variant, func(t *testing.T) {
			c := wifTestConfig()
			r := wifTestRequest(t, func(wf *wfv1.Workflow) {
				leaf := &wf.Spec.Templates[0]
				switch variant {
				case "workflow-optout":
					wf.Spec.AutomountServiceAccountToken = ptr.To(false)
				case "leaf-optout":
					leaf.AutomountServiceAccountToken = ptr.To(false)
				case "defaults":
					wf.Spec.TemplateDefaults = &wfv1.Template{AutomountServiceAccountToken: ptr.To(false)}
				case "dag":
					leaf.Container = nil
					leaf.DAG = &wfv1.DAGTemplate{}
				case "root-annotation":
					wf.Annotations[argoworkflow.ReleaseWIFRoleAnnotation] = "image-writer"
				case "role":
					leaf.Metadata.Annotations[argoworkflow.ReleaseWIFRoleAnnotation] = "chart-writer"
				case "leaf":
					leaf.Name = "other"
					wf.Spec.Entrypoint = "other"
				case "alias":
					leaf.Container.VolumeMounts = append(leaf.Container.VolumeMounts, corev1.VolumeMount{Name: releaseWIFVolume, MountPath: "/alias"})
				case "volume":
					wf.Spec.Volumes = append(wf.Spec.Volumes, corev1.Volume{Name: releaseWIFVolume})
				}
			})
			switch variant {
			case "disabled":
				c.ReleaseWIF = nil
			case "namespace":
				c.ReleaseWIF.Namespace = "foreign"
			case "ci-identity-alias":
				c.PerRepoCIIdentities = map[string]PerRepoIdentityConfig{"github/skipops/oberth": {ServiceAccountName: "oberth-per-repo"}}
			case "repo-identity-alias":
				c.PerRepoIdentities["github/skipops/foreign"] = PerRepoIdentityConfig{ServiceAccountName: "oberth-per-repo"}
			case "ci":
				r.Trigger = periapsis.TriggerCI
			case "repo":
				r.Repo = "other"
			case "upstream":
				r.UpstreamName = "codeberg"
			case "identity-missing":
				c.PerRepoIdentities = nil
			case "identity-changed":
				c.PerRepoIdentities["github/skipops/oberth"] = PerRepoIdentityConfig{ServiceAccountName: "different"}
			case "common-pool":
				c.ReleaseWIF.Roles["image-reader"] = c.ReleaseWIF.Roles["image-writer"]
			case "malicious-provider":
				role := c.ReleaseWIF.Roles["image-writer"]
				role.Provider = "https://attacker.example"
				c.ReleaseWIF.Roles["image-writer"] = role
			case "malicious-account":
				role := c.ReleaseWIF.Roles["image-writer"]
				role.ServiceAccount = "attacker@example.com/path"
				c.ReleaseWIF.Roles["image-writer"] = role
			}
			if _, err := Build(c, r); err == nil {
				t.Fatal("unapproved WIF authority accepted")
			}
		})
	}
}

func TestReleaseWIFDoesNotCredentialUnselectedLeaves(t *testing.T) {
	for _, mode := range []string{"default", "true", "false"} {
		t.Run(mode, func(t *testing.T) {
			r := wifTestRequest(t, func(wf *wfv1.Workflow) {
				other := wf.Spec.Templates[0].DeepCopy()
				other.Name = "test-only"
				other.Metadata.Annotations = nil
				other.AutomountServiceAccountToken = nil
				if mode != "default" {
					other.AutomountServiceAccountToken = ptr.To(mode == "true")
				}
				helper := wfv1.UserContainer{Container: corev1.Container{Name: "helper", Image: other.Container.Image, Command: []string{"true"}}, MirrorVolumeMounts: ptr.To(true)}
				other.Sidecars = []wfv1.UserContainer{helper}
				other.InitContainers = []wfv1.UserContainer{helper}
				wf.Spec.Templates = append(wf.Spec.Templates, *other)
				// The approved WIF leaf need not run. Its mere presence must
				// not widen the authority of the ordinary entrypoint.
				wf.Spec.Entrypoint = other.Name
			})
			wf, err := Build(wifTestConfig(), r)
			if err != nil {
				t.Fatal(err)
			}
			if wf.Spec.AutomountServiceAccountToken == nil || *wf.Spec.AutomountServiceAccountToken {
				t.Fatal("workflow retained ambient API-token authority")
			}
			other := &wf.Spec.Templates[1]
			if !tokenDisabled(other) {
				t.Fatal("ordinary sibling retained ambient API-token authority")
			}
			if other.Metadata.Annotations[releaseWIFADCAnnotation] != "" {
				t.Fatal("unselected leaf received ADC annotation")
			}
			for _, v := range other.Volumes {
				if v.Name == releaseWIFVolume {
					t.Fatal("unselected leaf received projection")
				}
			}
			templateContainers(other, func(c *corev1.Container) {
				for _, e := range c.Env {
					if e.Name == "GOOGLE_APPLICATION_CREDENTIALS" || e.Name == "OBERTH_RELEASE_WIF" {
						t.Fatal("unselected leaf received ADC environment")
					}
				}
				for _, m := range c.VolumeMounts {
					if m.Name == releaseWIFVolume || m.Name == ReleaseTokenVolumeName {
						t.Fatal("ordinary sibling received credential projection")
					}
				}
			})
		})
	}
}

func TestReleaseWIFRetainsExplicitApprovedVaultProjection(t *testing.T) {
	const secret = "oberth/data/release/r2-upload-token"
	r := wifTestRequest(t, func(wf *wfv1.Workflow) {
		wf.Annotations[argoworkflow.SecretPathsAnnotation] = secret
		other := wf.Spec.Templates[0].DeepCopy()
		other.Name = "vault-publisher"
		other.Metadata.Annotations = nil
		other.Container.Command = []string{"oberth", "secretstore", "exec", "--dir=/run/oberth-secrets", "--path=" + secret, "--", "/bin/true"}
		wf.Spec.Templates = append(wf.Spec.Templates, *other)
	})
	r.ApprovedSecrets[secret] = true
	wf, err := Build(wifTestConfig(), r)
	if err != nil {
		t.Fatal(err)
	}
	other := &wf.Spec.Templates[1]
	if !tokenDisabled(other) {
		t.Fatal("Vault sibling retained automatic API-token mount")
	}
	found := false
	for _, m := range other.Container.VolumeMounts {
		if m.Name == ReleaseTokenVolumeName {
			found = true
		}
	}
	if !found {
		t.Fatal("approved explicit Vault projection removed")
	}
}
