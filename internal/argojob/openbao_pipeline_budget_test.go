package argojob

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/pkg/periapsis"
)

func TestOpenBaoPipelinePhasesPreserveTestsAndStopOnFailure(t *testing.T) {
	workflow, err := buildOberthPipelineForIsolation(t, periapsis.TriggerCI, loadPipeline(t, "build.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var script string
	for _, leaf := range workflow.Spec.Templates {
		if leaf.Name != "test-openbao" {
			continue
		}
		if leaf.ActiveDeadlineSeconds == nil || leaf.ActiveDeadlineSeconds.IntValue() != 780 || leaf.Script.Resources.Limits.Memory().Value() != 1<<30 || leaf.Script.Resources.Limits.Cpu().MilliValue() != 2000 {
			t.Fatal("outer resources changed")
		}
		script = leaf.Script.Source
	}
	if script == "" {
		t.Fatal("missing actual leaf")
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
[[ "$*" == *'--max-time 40'* ]]
[[ "$FAIL_PHASE" != download ]]
`)
			write("sha256sum", `printf 'checksum\n' >> "$TRACE"
[[ "$FAIL_PHASE" != checksum ]]
`)
			write("tar", `printf '#!/bin/sh\nexit 0\n' > bao
`)
			write("go", `printf 'compile\n' >> "$TRACE"
[[ "$GOMAXPROCS" == 2 && "$GOMEMLIMIT" == 256MiB && "$CGO_ENABLED" == 1 && "$GOTOOLCHAIN" == local && "$GOWORK" == off && "$GOENV" == off ]]
[[ "$1" == test && "$2" == -mod=readonly && "$3" == -p=1 && "$4" == -tags=openbao_integration && "$5" == -race && "$6" == -c && "$7" == -o && "$9" == ./internal/installer/ ]]
[[ "$FAIL_PHASE" != compile ]]
cat > "$8" <<'CHILD'
#!/bin/bash
set -euo pipefail
printf 'test\n' >> "$TRACE"
[[ "$*" == '-test.count=1 -test.v -test.timeout=120s -test.run=TestPolicyIsolation' ]]
[[ "$PWD" == "$EXPECTED_PACKAGE" ]]
[[ "$FAIL_PHASE" != test ]]
CHILD
chmod +x "$8"
`)
			source := strings.ReplaceAll(script, "/work/src", src)
			source = strings.Replace(source, `BAO_DIR="/tmp/openbao"`, `BAO_DIR="`+bao+`"`, 1)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
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
