package argojob

import (
	"os/exec"
	"reflect"
	"strings"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"

	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/periapsis"
)

func TestBootstrapPublisherSourceAndToolSubstitution(t *testing.T) {
	command := exec.CommandContext(t.Context(), "/usr/bin/python3", "-I", "-S", "-B", "../../hack/test-release-tools.py")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("publisher source/tool substitution controls: %v\n%s", err, output)
	}
}

func TestBootstrapRetainsTestsAndSeparatesPublisherAuthority(t *testing.T) {
	workflow, err := argoworkflow.Decode(loadPipeline(t, "release.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	tests := map[string][]string{
		"vet":                {"/tmp/oberth-tools/bin/go", "vet", "./..."},
		"golangci-lint-run":  {"/tmp/oberth-tools/bin/golangci-lint", "run", "--timeout=10m", "--concurrency=1", "./..."},
		"release-test":       {"/tmp/oberth-tools/bin/go", "test", "-race", "-v", "-count=1", "./..."},
		"release-chart-test": {"./hack/test-chart.sh"},
		"release-scan": {"/usr/local/bin/trivy", "fs", "--cache-dir", "/tmp/oberth-trivy", "--no-progress", "--skip-check-update",
			"--skip-vex-repo-update", "--skip-version-check", "--disable-telemetry", "--scanners", "vuln", "--exit-code", "1", "--severity", "HIGH,CRITICAL", "--skip-dirs", ".git", "."},
	}
	for _, claim := range workflow.Spec.VolumeClaimTemplates {
		if claim.Name == "work" {
			t.Fatal("reserved work claim restores implicit writable aliases")
		}
	}
	seenTests, credentialed := 0, 0
	for _, template := range workflow.Spec.Templates {
		container := template.Container
		if container == nil {
			continue
		}
		want, isTest := tests[template.Name]
		if isTest {
			seenTests++
			if len(container.Args) < 2 || !reflect.DeepEqual(container.Args[2:], want) {
				t.Errorf("%s changed an existing test command", template.Name)
			}
		}
		for _, mount := range container.VolumeMounts {
			if (isTest || template.Name == "release-runtime" || template.Name == "release-build") && mount.Name == "bootstrap-tools" {
				t.Errorf("%s can reach the trusted publisher tools claim", template.Name)
			}
			if (isTest || template.Name == "release-runtime") && mount.Name == "bootstrap-output" {
				t.Errorf("%s can reach candidate publisher output", template.Name)
			}
			if isTest && mount.Name == "bootstrap-runtime-receipts" {
				t.Errorf("%s can forge the later runtime receipt", template.Name)
			}
		}
		if templateUsesOberthSecretstore(&template) {
			credentialed++
			// The website leaf verifies its own source and staged inputs and
			// installs wrangler offline before its secret exists; it never
			// receives only verified, read-only publisher tools and inputs.
			wantInit := []string{"verify-publisher-tools"}
			if template.Name == "release-website" {
				wantInit = []string{"verify-publisher-tools", "verify-website-inputs", "install-website-tools"}
			}
			var gotInit []string
			for _, container := range template.InitContainers {
				gotInit = append(gotInit, container.Name)
			}
			if !reflect.DeepEqual(gotInit, wantInit) {
				t.Errorf("%s lost its pre-secret verifier: init containers %v, want %v", template.Name, gotInit, wantInit)
			}
		}
		if template.Name == "release-verify" {
			paths := extractExecPaths(&template)
			if !reflect.DeepEqual(paths, []string{"oberth/data/release/gar-reader-key"}) {
				t.Errorf("verification fetched unnecessary credentials: %v", paths)
			}
		}
		for _, publisher := range []struct {
			name, key string
		}{
			{"release-publish-images", "gar-image-key"},
			{"release-publish-chart", "gar-chart-key"},
		} {
			if template.Name != publisher.name {
				continue
			}
			paths := extractExecPaths(&template)
			if len(paths) == 0 || paths[0] != "oberth/data/release/"+publisher.key {
				t.Errorf("%s does not fetch its exact publisher credential: %v", template.Name, paths)
			}
			for _, path := range paths[1:] {
				if strings.HasPrefix(path, "oberth/data/release/gar-") {
					t.Errorf("%s fetched another GAR principal: %v", template.Name, paths)
				}
			}
		}
		if template.Name == "release-website" {
			if template.Synchronization == nil || len(template.Synchronization.Mutexes) != 1 || template.Synchronization.Mutexes[0].Name != "oberth-release-finalize" {
				t.Fatal("finalizers no longer share the fixed alias-publication mutex")
			}
		}
	}
	if seenTests != 5 || credentialed != 6 {
		t.Fatalf("retained tests=%d credentialed leaves=%d, want 5 and 6", seenTests, credentialed)
	}
}

func TestPublisherVerifierHasCredentialFreeRetainedLogGate(t *testing.T) {
	workflow, err := argoworkflow.Decode(loadPipeline(t, "release.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var verifier *wfv1.Template
	for index := range workflow.Spec.Templates {
		if workflow.Spec.Templates[index].Name == "verify-publisher-tools" {
			verifier = &workflow.Spec.Templates[index]
			break
		}
	}
	if verifier == nil || verifier.Container == nil {
		t.Fatal("release has no retained-log publisher verifier")
	}
	if templateUsesOberthSecretstore(verifier) || len(verifier.InitContainers) != 0 {
		t.Fatal("publisher verifier must be a credential-free main-container step")
	}
	if verifier.AutomountServiceAccountToken == nil || *verifier.AutomountServiceAccountToken {
		t.Fatal("publisher verifier must not automount a service account token")
	}
	foundReadOnlyTools := false
	for _, mount := range verifier.Container.VolumeMounts {
		if mount.Name == "bootstrap-tools" && mount.MountPath == "/tmp/oberth-tools" && mount.ReadOnly {
			foundReadOnlyTools = true
		}
	}
	if !foundReadOnlyTools {
		t.Fatal("publisher verifier must inspect the shared tool claim read-only")
	}
	if !strings.Contains(strings.Join(verifier.Container.Args, "\n"), "/work/src/.oberth/verify-release-tools.py") {
		t.Fatal("publisher verifier does not run the exact release verifier")
	}
	var buildDependency string
	for _, template := range workflow.Spec.Templates {
		if template.Name != "release" || template.DAG == nil {
			continue
		}
		for _, task := range template.DAG.Tasks {
			if task.Name == "release-build" {
				buildDependency = task.Depends
			}
		}
	}
	if !strings.Contains(buildDependency, "release-verify-publisher-tools") {
		t.Fatalf("release-build does not wait for retained-log publisher verifier: %q", buildDependency)
	}
}

// Execute the actual wrappers after Build has injected cache and run variables.
// Merely checking authored env fields misses the deployed server's overrides.
func TestBootstrapExecutedEnvironmentIgnoresSharedCachesAndCredentials(t *testing.T) {
	config := oberthConfig()
	config.ReleaseCacheRoot = "/var/cache/oberth/release"
	workflow, err := Build(config, Request{
		RunID: "bootstrap-environment", Name: "oberth-bootstrap-environment",
		Repo: "oberth", UpstreamOrg: "skipops", Ref: "v1.2.3", SHA: testSHA,
		Trigger: periapsis.TriggerRelease, Source: loadPipeline(t, "release.yaml"),
		ApprovedSecrets: oberthReleaseApprovedSecrets(),
		SourceVolume:    SourceVolume{ClaimName: "source-fixture", SubPath: "source", ArtifactsSubPath: "artifacts"},
	})
	if err != nil {
		t.Fatal(err)
	}
	tested := 0
	for _, template := range workflow.Spec.Templates {
		container := template.Container
		if container == nil || template.Name == "copy-test-tools" {
			continue
		}
		t.Run(template.Name, func(t *testing.T) {
			credentialed := templateUsesOberthSecretstore(&template)
			args := container.Args
			if credentialed {
				separator := -1
				for index, argument := range args {
					if argument == "--" {
						separator = index
						break
					}
				}
				if separator < 0 || len(args) < separator+6 || args[separator+1] != "/bin/sh" || args[separator+2] != "-c" {
					t.Fatal("credentialed command lost its environment wrapper")
				}
				args = args[separator+3:]
			} else if !reflect.DeepEqual(container.Command, []string{"/bin/sh", "-c"}) {
				t.Fatal("command lost its environment wrapper")
			}
			command := exec.CommandContext(t.Context(), "/bin/sh", "-c", args[0], args[1], "/usr/bin/env")
			for _, item := range container.Env {
				command.Env = append(command.Env, item.Name+"="+item.Value)
			}
			command.Env = append(command.Env,
				"OBERTH_SECRETSTORE_DIR=/run/oberth-secrets", "COSIGN_KEY=synthetic-key",
				"COSIGN_PASSWORD=synthetic-password", "VAULT_TOKEN=synthetic-token",
				"GOOGLE_APPLICATION_CREDENTIALS=/work/cache/poison.json",
				"GOFLAGS=-toolexec=/work/cache/poison", "PYTHONPATH=/work/artifacts/poison")
			output, err := command.Output()
			if err != nil {
				t.Fatal(err)
			}
			environment := map[string]string{}
			for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
				name, value, ok := strings.Cut(line, "=")
				if !ok {
					t.Fatal("malformed environment output")
				}
				environment[name] = value
				if strings.Contains(value, "/work/cache") || strings.Contains(value, "/work/artifacts") {
					t.Errorf("executed command consumes shared path through %s", name)
				}
			}
			for _, key := range []string{"COSIGN_KEY", "COSIGN_PASSWORD", "VAULT_TOKEN", "VAULT_ADDR", "OBERTH_VAULT_ROLE", "GOOGLE_APPLICATION_CREDENTIALS", "PYTHONPATH"} {
				if _, exists := environment[key]; exists {
					t.Errorf("executed command inherited %s", key)
				}
			}
			if _, exists := environment["OBERTH_SECRETSTORE_DIR"]; exists != credentialed {
				t.Fatal("credential root reached the wrong execution class")
			}
			if credentialed && environment["PATH"] != "/usr/bin:/bin" {
				t.Fatal("credentialed PATH permits shared tool substitution")
			}
			if template.Name == "verify-publisher-tools" {
				if environment["PATH"] != "/usr/bin:/bin" || environment["HOME"] != "/tmp" {
					t.Fatal("publisher verifier environment is not fixed")
				}
				if _, exists := environment["OBERTH_TOOLS_DIR"]; exists {
					t.Fatal("publisher verifier inherited tool-directory overrides")
				}
				return
			}
			if environment["OBERTH_TOOLS_DIR"] != "/tmp/oberth-tools" || environment["GOCACHE"] != "/tmp/bootstrap-private/gobuild" || environment["GOMODCACHE"] != "/tmp/bootstrap-private/gomod" {
				t.Fatal("tool/cache roots are not fixed")
			}
		})
		tested++
	}
	// 25 bootstrap wrappers plus the five website templates of #649
	// (fetch-node, verify-node, stage-website-packages, release-website,
	// release-website-readback).
	if tested != 30 {
		t.Fatalf("executed %d wrappers, want all 30", tested)
	}
}
