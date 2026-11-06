package argojob

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/testtimeout"
	"github.com/oberthci/oberth/pkg/pipeline"
)

func TestOpenBaoPipelinePhasesPreserveTestsAndStopOnFailure(t *testing.T) {
	workflow, err := buildOberthPipelineForIsolation(t, pipeline.TriggerCI, loadPipeline(t, "build.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var script string
	for _, leaf := range workflow.Spec.Templates {
		if leaf.Name != "test-openbao" {
			continue
		}
		if leaf.ActiveDeadlineSeconds == nil || leaf.ActiveDeadlineSeconds.IntValue() != 45+180+120+26 || leaf.Script.Resources.Limits.Memory().Value() < 4<<30 || leaf.Script.Resources.Requests.Memory().Value() < 2<<30 || leaf.Script.Resources.Limits.Cpu().MilliValue() < 2000 {
			t.Fatal("outer resources fell below the retained phase budgets")
		}
		script = leaf.Script.Source
	}
	if script == "" {
		t.Fatal("missing actual leaf")
	}
	for _, bound := range []string{"timeout --signal=TERM --kill-after=5s 40s", "timeout --signal=TERM --kill-after=5s 175s"} {
		if !strings.Contains(script, bound) {
			t.Fatalf("missing phase bound %q", bound)
		}
	}
	for _, phase := range []string{"success", "download", "checksum", "compile", "test"} {
		t.Run(phase, func(t *testing.T) {
			dir := t.TempDir()
			bin := filepath.Join(dir, "bin")
			src := filepath.Join(dir, "src")
			bao := filepath.Join(dir, "bao")
			trace := filepath.Join(dir, "trace")
			for _, path := range []string{bin, filepath.Join(src, "internal/installer"), filepath.Join(src, ".oberth/pins")} {
				if err := os.MkdirAll(path, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			write := func(name, body string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/bash\nset -euo pipefail\n"+body), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			write("curl", `printf 'download\n' >> "$TRACE"
[[ "$*" == *'--max-time 40'* ]] || exit 1
[[ "$FAIL_PHASE" != download ]] || exit 1
`)
			write("sha256sum", `printf 'checksum\n' >> "$TRACE"
[[ "$FAIL_PHASE" != checksum ]] || exit 1
`)
			write("tar", `printf '#!/bin/sh\nexit 0\n' > bao
`)
			write("go", `printf 'compile\n' >> "$TRACE"
[[ "$GOMAXPROCS" == 2 && "$GOMEMLIMIT" == 256MiB && "$CGO_ENABLED" == 1 && "$GOTOOLCHAIN" == local && "$GOWORK" == off && "$GOENV" == off ]] || exit 1
[[ "$1" == test && "$2" == -mod=readonly && "$3" == -p=1 && "$4" == -tags=openbao_integration && "$5" == -c && "$6" == -o && "$8" == ./internal/installer/ ]] || exit 1
[[ "$FAIL_PHASE" != compile ]] || exit 1
cat > "$7" <<'CHILD'
#!/bin/bash
set -euo pipefail
printf 'test\n' >> "$TRACE"
[[ "$*" == '-test.count=1 -test.v -test.timeout=120s -test.run=^(TestPolicyIsolation.*|TestSetupSecretStoreOpenBao)$' ]] || exit 1
[[ "$PWD" == "$EXPECTED_PACKAGE" ]] || exit 1
[[ "$FAIL_PHASE" != test ]] || exit 1
CHILD
chmod +x "$7"
`)
			source := strings.ReplaceAll(script, "/work/src", src)
			source = strings.Replace(source, `BAO_DIR="/tmp/openbao"`, `BAO_DIR="`+bao+`"`, 1)
			ctx, cancel := context.WithTimeout(context.Background(), testtimeout.Start(5*time.Second))
			defer cancel()
			cmd := exec.CommandContext(ctx, "/bin/bash", "-c", source)
			cmd.Env = []string{"PATH=" + bin + ":/usr/bin:/bin", "HOME=" + dir, "FAIL_PHASE=" + phase, "TRACE=" + trace, "EXPECTED_PACKAGE=" + filepath.Join(src, "internal/installer")}
			cmd.Dir = src
			out, runErr := cmd.CombinedOutput()
			if ctx.Err() != nil {
				t.Fatal("fixture exceeded bound")
			}
			if (runErr == nil) != (phase == "success") {
				t.Fatalf("phase %s result %v output %s", phase, runErr, out)
			}
			body, err := os.ReadFile(trace)
			if err != nil {
				t.Fatal(err)
			}
			want := map[string]string{"download": "download\n", "checksum": "download\nchecksum\n", "compile": "download\nchecksum\ncompile\n", "test": "download\nchecksum\ncompile\ntest\n", "success": "download\nchecksum\ncompile\ntest\n"}[phase]
			if string(body) != want {
				t.Fatalf("phase advanced/retried after failure: %q", body)
			}
			if phase == "success" && !strings.Contains(string(out), "openbao-phase=complete") {
				t.Fatal("success not complete")
			}
		})
	}
}

func TestOberthOpenBaoUsesVerifiedGoCachesWithoutWritableTools(t *testing.T) {
	for _, persistent := range []bool{false, true} {
		t.Run(map[bool]string{false: "per-run", true: "persistent"}[persistent], func(t *testing.T) {
			config := oberthConfig()
			if persistent {
				config.CICacheRoot = "/var/lib/oberth-cache/ci"
			}
			workflow, err := buildOberthPipeline(config, Request{
				RunID: "openbao-cache", Name: "openbao-cache", Repo: "oberth", UpstreamOrg: "acme", UpstreamName: "github",
				Ref: "refs/heads/main", SHA: testSHA, Trigger: pipeline.TriggerCI, Source: loadPipeline(t, "build.yaml"), ApprovedSecrets: map[string]bool{},
				SourceVolume: SourceVolume{ClaimName: "oberth-source-run", SubPath: "run/src"},
			})
			if err != nil {
				t.Fatal(err)
			}
			for _, leaf := range workflow.Spec.Templates {
				if leaf.Name != "test-openbao" {
					continue
				}
				if leaf.Script == nil || !tokenless(&leaf) {
					t.Fatal("OpenBao lost its tokenless script")
				}
				if err := credentialMaterial(&leaf); err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(leaf.Script.Source, "go test -mod=readonly -p=1 -tags=openbao_integration -c") || strings.Contains(leaf.Script.Source, "-race -c") || !strings.Contains(leaf.Script.Source, "sha256sum -c /work/src/.oberth/pins/openbao.sha256") {
					t.Fatal("OpenBao compilation or verified release download drifted")
				}
				env := map[string]string{}
				for _, e := range leaf.Script.Env {
					env[e.Name] = e.Value
				}
				wantMod, wantBuild := WorkspaceGomodFallbackPath, WorkspaceGobuildFallbackPath
				if persistent {
					wantMod, wantBuild = ModuleCachePath, BuildCachePath
				}
				if env["GOMODCACHE"] != wantMod || env["GOCACHE"] != wantBuild {
					t.Fatalf("cache paths = %v", env)
				}
				mounts := map[string]bool{}
				for _, mount := range leaf.Script.VolumeMounts {
					switch mount.MountPath {
					case WorkspaceToolsMountPath:
						if !mount.ReadOnly || mount.SubPath != "tools" {
							t.Fatal("setup tools are writable or not isolated")
						}
						mounts["tools"] = true
					case WorkspaceGomodFallbackPath, WorkspaceGobuildFallbackPath:
						want := "gomod"
						if mount.MountPath == WorkspaceGobuildFallbackPath {
							want = "gobuild"
						}
						if mount.Name != WorkspaceVolumeName || mount.SubPath != want || mount.ReadOnly {
							t.Fatalf("wrong fallback cache: %+v", mount)
						}
						mounts[want] = true
					case CacheMountPath:
						if mount.Name != CacheVolumeName || mount.ReadOnly {
							t.Fatalf("wrong persistent cache: %+v", mount)
						}
						mounts["persistent"] = true
					}
				}
				if !mounts["tools"] || !mounts["gomod"] || !mounts["gobuild"] || mounts["persistent"] != persistent {
					t.Fatalf("missing cache/tool mount: %v", mounts)
				}
				return
			}
			t.Fatal("missing OpenBao leaf")
		})
	}
}
