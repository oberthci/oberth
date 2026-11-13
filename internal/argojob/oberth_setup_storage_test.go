package argojob

import (
	"testing"

	"github.com/oberthci/oberth/pkg/pipeline"
)

// The installed binaries and caches live on persistent volumes, but Go's
// intermediate compiler/linker files otherwise default to the Pod's /tmp.
// Verify the admitted setup leaves, including the no-server-cache fallback.
func TestOberthGoToolSetupScratchUsesPrivateWorkClaim(t *testing.T) {
	for _, cacheRoot := range []string{"", "/var/lib/ci-cache"} {
		t.Run(cacheRoot, func(t *testing.T) {
			config := oberthConfig()
			config.CICacheRoot = cacheRoot
			workflow, err := Build(config, Request{
				RunID: "setup-storage", Name: "setup-storage", Repo: "oberth", UpstreamOrg: "acme", UpstreamName: "github",
				Ref: "refs/heads/main", SHA: testSHA, Trigger: pipeline.TriggerCI, Source: loadPipeline(t, "build.yaml"), ApprovedSecrets: map[string]bool{},
			})
			if err != nil {
				t.Fatal(err)
			}
			claim := false
			for _, volume := range workflow.Spec.VolumeClaimTemplates {
				claim = claim || volume.Name == "work"
			}
			if !claim {
				t.Fatal("compiler scratch requires the existing bounded work claim")
			}
			for _, name := range []string{"install-golangci-lint", "install-deadcode", "install-helm"} {
				found := false
				for _, template := range workflow.Spec.Templates {
					if template.Name != name {
						continue
					}
					found = true
					container := template.Container
					if container == nil {
						t.Fatalf("%s lost its tool installer", name)
					}
					env := map[string]string{}
					for _, value := range container.Env {
						env[value.Name] = value.Value
					}
					if env["GOTMPDIR"] == "" || env["GOTMPDIR"] != env["TMPDIR"] {
						t.Errorf("%s: Go and child scratch must explicitly use the same private claim directory", name)
					}
					backed := false
					for _, mount := range container.VolumeMounts {
						if mount.MountPath == env["GOTMPDIR"] && mount.Name == "work" && !mount.ReadOnly && mount.SubPath == "go-tmp/"+name {
							backed = true
						}
					}
					if !backed {
						t.Errorf("%s: compiler scratch is not on its own writable work-claim subpath", name)
					}
					if name == "install-helm" && container.Resources.Limits.StorageEphemeral().Value() < 96<<20 {
						t.Error("the evicted 64Mi local-storage cap still needs its one-time 1.5x remeasurement increase")
					}
				}
				if !found {
					t.Fatalf("missing setup installer %s", name)
				}
			}
		})
	}
}
