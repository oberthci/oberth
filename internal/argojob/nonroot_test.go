package argojob

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"

	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/periapsis"
)

const nonrootTestDocument = `apiVersion: argoproj.io/v1alpha1
kind: Workflow
metadata:
  annotations:
    oberth.ci/nonroot-templates: test
spec:
  entrypoint: test
  activeDeadlineSeconds: 600
  templates:
  - name: test
    container:
      image: golang:1.26.6-trixie@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
      command: [/bin/true]
      env:
      - {name: GOCACHE, value: /bad/shared}
      - {name: GOTMPDIR, value: /bad/long/path}
  - name: ordinary
    container:
      image: golang:1.26.6-trixie@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
      command: [/bin/true]
`

func nonrootBuiltFixture(t *testing.T, script bool) *wfv1.Workflow {
	t.Helper()
	document := nonrootTestDocument
	if script {
		wf, err := argoworkflow.Decode([]byte(document))
		if err != nil {
			t.Fatal(err)
		}
		tmpl := &wf.Spec.Templates[0]
		tmpl.Script = &wfv1.ScriptTemplate{Container: *tmpl.Container, Source: "true"}
		tmpl.Script.Command = []string{"/bin/sh"}
		tmpl.Container = nil
		encoded, err := json.Marshal(wf)
		if err != nil {
			t.Fatal(err)
		}
		document = string(encoded)
	}
	config := testConfig()
	config.NonrootProfile = argoworkflow.NonrootStaticProfile
	config.CICacheRoot = "/server/cache/ci"
	config.ReleaseCacheRoot = "/server/cache/release"
	request := testRequest(periapsis.TriggerCI, document)
	request.SourceVolume = SourceVolume{ClaimName: "run-source", SubPath: "src", ArtifactsSubPath: "artifacts"}
	request = nonrootFixtureProof(request)
	wf, err := Build(config, request)
	if err != nil {
		t.Fatal(err)
	}
	return wf
}

func nonrootFixtureProof(request Request) Request {
	request.nonrootProof = &nonrootProof{requestID: nonrootRequestIdentity(request), namespace: testNamespace, podUID: "fixture-controller", configUID: "fixture-config", configVersion: "1", verifiedAt: time.Now()}
	return request
}

func TestBuildNonrootLeaves(t *testing.T) {
	for _, script := range []bool{false, true} {
		wf := nonrootBuiltFixture(t, script)
		tmpl := &wf.Spec.Templates[0]
		if tmpl.SecurityContext == nil || *tmpl.SecurityContext.RunAsUser != nonrootUID || !*tmpl.SecurityContext.RunAsNonRoot {
			t.Fatal("missing fixed nonroot identity")
		}
		if wf.Spec.Templates[1].SecurityContext != nil || wf.Spec.Templates[1].PodSpecPatch != "" || *wf.Spec.SecurityContext.RunAsUser != 0 {
			t.Fatal("ordinary leaf changed")
		}
		container := tmpl.Container
		if script {
			container = &tmpl.Script.Container
		}
		for _, mount := range container.VolumeMounts {
			if mount.Name == CacheVolumeName {
				t.Fatal("persistent cache is mounted")
			}
			if mount.Name != nonrootTmpVolume && mount.Name != ArtifactsVolumeName && !mount.ReadOnly {
				t.Fatalf("input writable: %s", mount.Name)
			}
		}
		wantEnv := map[string]string{"TMPDIR": "/tmp", "GOTMPDIR": "/tmp", "GOCACHE": "/tmp/gobuild", "GOMODCACHE": "/tmp/gomod"}
		for _, env := range container.Env {
			if value, ok := wantEnv[env.Name]; ok {
				if env.Value != value {
					t.Errorf("%s=%s", env.Name, env.Value)
				}
				delete(wantEnv, env.Name)
			}
		}
		if len(wantEnv) != 0 {
			t.Fatalf("missing environment: %v", wantEnv)
		}
		if tmpl.PodSpecPatch == "" {
			t.Fatal("no executor policy")
		}
	}
}

func TestNonrootControllerSupportFailsClosed(t *testing.T) {
	_, err := Build(testConfig(), testRequest(periapsis.TriggerCI, nonrootTestDocument))
	if err == nil || !strings.Contains(err.Error(), "support has not been established") {
		t.Fatalf("unverified controller accepted selected leaves: %v", err)
	}
}

// The overlay driver requests these actual Build outputs in its fresh private
// directory. Ordinary unit runs still validate both fixtures without writing
// outside t.TempDir. No production endpoint or test-only server API is added.
func TestNonrootControllerFixtures(t *testing.T) {
	directory := t.TempDir()
	if export := os.Getenv("OBERTH_NONROOT_FIXTURE_DIR"); export != "" {
		directory = export
	}
	for _, fixture := range []struct {
		name   string
		script bool
	}{{"container", false}, {"script", true}} {
		data, err := json.Marshal(nonrootBuiltFixture(t, fixture.script))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(directory, fixture.name+".json"), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestNonrootPreparationMountsOnlyPrivateScratch(t *testing.T) {
	wf := nonrootBuiltFixture(t, true)
	var patch struct {
		InitContainers []corev1.Container `json:"initContainers"`
	}
	if err := json.Unmarshal([]byte(wf.Spec.Templates[0].PodSpecPatch), &patch); err != nil {
		t.Fatal(err)
	}
	prepare := patch.InitContainers[0]
	if prepare.Name != nonrootPrepareName || prepare.Image != defaultSourceSeedImage || *prepare.SecurityContext.RunAsUser != 0 {
		t.Fatal("initializer is not fixed server-owned preparation")
	}
	if len(prepare.VolumeMounts) != 4 {
		t.Fatalf("preparation mounts=%d", len(prepare.VolumeMounts))
	}
	for _, mount := range prepare.VolumeMounts {
		if mount.Name != nonrootTmpVolume && mount.Name != "var-run-argo" && mount.Name != "tmp-dir-argo" && mount.Name != "argo-staging" {
			t.Fatalf("initializer can access %s", mount.Name)
		}
		if mount.SubPath != "" || mount.SubPathExpr != "" {
			t.Fatal("initializer mounts a preexisting subpath")
		}
	}
}

func TestNonrootPreparationRequiresPinnedImage(t *testing.T) {
	wf := nonrootBuiltFixture(t, false)
	for _, image := range []string{"", "busybox:latest", "busybox:1.37"} {
		if _, err := nonrootPodPatch(&wf.Spec.Templates[0], image); err == nil {
			t.Fatalf("accepted preparation image %q", image)
		}
	}
}

func TestNonrootPreparationCanReplayWithoutAdoptingOtherPaths(t *testing.T) {
	wf := nonrootBuiltFixture(t, true)
	var patch struct {
		InitContainers []corev1.Container `json:"initContainers"`
	}
	if err := json.Unmarshal([]byte(wf.Spec.Templates[0].PodSpecPatch), &patch); err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []string{"fresh", "partial", "symlink", "file"} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			for _, name := range []string{"tmp", "argo", "executor", "staging"} {
				if err := os.Mkdir(filepath.Join(root, name), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			target := filepath.Join(root, "executor", "0")
			switch scenario {
			case "partial":
				if err := os.Mkdir(target, 0o755); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Symlink(filepath.Join(root, "tmp"), target); err != nil {
					t.Fatal(err)
				}
			case "file":
				if err := os.WriteFile(target, nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			// Paths are generated by testing, shell-quoted rather than interpolated
			// as code. The command itself comes directly from CURRENT Build.
			command := strings.ReplaceAll(patch.InitContainers[0].Command[2], "/prepare/", "'"+strings.ReplaceAll(root, "'", "'\\''")+"'/")
			for attempt := 0; attempt < 2; attempt++ {
				output, err := exec.CommandContext(t.Context(), "/bin/sh", "-ec", command).CombinedOutput()
				if scenario == "symlink" || scenario == "file" {
					if err == nil {
						t.Fatalf("adopted %s on attempt %d", scenario, attempt)
					}
					info, statErr := os.Stat(filepath.Join(root, "tmp"))
					if statErr != nil || info.Mode().Perm() != 0o755 {
						t.Fatal("invalid path caused partial chmod")
					}
					continue
				}
				if err != nil {
					t.Fatalf("attempt %d: %v: %s", attempt, err, output)
				}
				for _, name := range []string{"tmp", "argo", "executor/0", "staging"} {
					info, statErr := os.Stat(filepath.Join(root, name))
					if statErr != nil || info.Mode().Perm() != 0o777 || info.Mode()&os.ModeSticky == 0 {
						t.Fatalf("private scratch %s not prepared", name)
					}
				}
			}
		})
	}
}

func TestNonrootSizeIsRecheckedAfterServerInjection(t *testing.T) {
	base := nonrootBuiltFixture(t, true)
	encoded, err := json.Marshal(&base.Spec.Templates[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, delta := range []int{-1, 0, 1} {
		wf, err := argoworkflow.Decode([]byte(nonrootTestDocument))
		if err != nil {
			t.Fatal(err)
		}
		tmpl := &wf.Spec.Templates[0]
		tmpl.Script = &wfv1.ScriptTemplate{Container: *tmpl.Container, Source: strings.Repeat("x", argoworkflow.MaxNonrootTemplateBytes-len(encoded)+4+delta)}
		tmpl.Script.Command = []string{"/bin/sh"}
		tmpl.Container = nil
		if err := argoworkflow.Admit(wf, argoworkflow.Policy{}); err != nil {
			t.Fatalf("authored size should fit before injection: %v", err)
		}
		document, err := json.Marshal(wf)
		if err != nil {
			t.Fatal(err)
		}
		config := testConfig()
		config.NonrootProfile = argoworkflow.NonrootStaticProfile
		config.CICacheRoot = "/server/cache/ci"
		config.ReleaseCacheRoot = "/server/cache/release"
		request := testRequest(periapsis.TriggerCI, string(document))
		request.SourceVolume = SourceVolume{ClaimName: "run-source", SubPath: "src", ArtifactsSubPath: "artifacts"}
		request = nonrootFixtureProof(request)
		_, err = Build(config, request)
		if (err != nil) != (delta > 0) {
			t.Fatalf("final size delta%d: %v", delta, err)
		}
	}
}
