package argojob

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/testtimeout"
	"github.com/oberthci/oberth/pkg/periapsis"
)

func TestOberthLintKeepsDeadlineCacheAndToolIsolation(t *testing.T) {
	for _, cacheRoot := range []string{"", "/var/lib/oberth-cache/ci"} {
		t.Run(cacheRoot, func(t *testing.T) {
			config := oberthConfig()
			config.CICacheRoot = cacheRoot
			workflow, err := buildOberthPipeline(config, Request{
				RunID: "run-lint", Name: "oberth-lint", Repo: "oberth", UpstreamOrg: "skipops", UpstreamName: "github",
				Ref: "refs/heads/main", SHA: testSHA, Trigger: periapsis.TriggerCI, Source: loadPipeline(t, "build.yaml"), ApprovedSecrets: map[string]bool{},
			})
			if err != nil {
				t.Fatal(err)
			}
			for _, template := range workflow.Spec.Templates {
				if template.Name == "ci" {
					for _, task := range template.DAG.Tasks {
						if task.Name == "lint" || (strings.HasPrefix(task.Name, "build-") && task.Name != "build-static-git-runtime") {
							if task.When != "" || task.ContinueOn != nil {
								t.Fatalf("%s must remain unconditional and enforcing", task.Name)
							}
						}
					}
				}
				if template.Name != "golangci-lint-run" {
					continue
				}
				container := template.Container
				want := []string{"hack/run-ci-lint.sh", "/tmp/oberth-tools/bin/golangci-lint", "run", "--timeout=10m", "--concurrency=1", "./..."}
				if container == nil || !reflect.DeepEqual(usageOriginalCommand(container.Command), []string{"/bin/sh"}) || !reflect.DeepEqual(container.Args, want) {
					t.Fatalf("lint lost its bounded launcher or checks: %+v", container)
				}
				env := map[string]string{}
				for _, variable := range container.Env {
					env[variable.Name] = variable.Value
				}
				if env["GOMEMLIMIT"] != "6GiB" || container.Resources.Limits.Memory().Value() != 8<<30 {
					t.Fatal("lint must leave memory headroom within the existing 8Gi limit")
				}
				wantCache := ""
				if cacheRoot != "" {
					wantCache = CacheMountPath
				}
				if env["OBERTH_CACHE_DIR"] != wantCache {
					t.Fatalf("cache advertised without the matching configuration: %v", env)
				}
				return
			}
			t.Fatal("missing lint leaf")
		})
	}
}

func lintLauncher(t *testing.T) string {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("GNU timeout from the pinned Linux CI image is required")
	}
	if _, err := os.Stat("/usr/bin/timeout"); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(repositoryRoot(t), "hack/run-ci-lint.sh")
}

func TestOberthLintLauncherPreservesFailureAndCache(t *testing.T) {
	launcher := lintLauncher(t)
	root := t.TempDir()
	fixture := filepath.Join(root, "linter")
	body := "#!/bin/sh\nprintf '%s\\n' \"$GOLANGCI_LINT_CACHE\" \"$@\"\nexit \"$LINT_TEST_EXIT\"\n"
	if err := os.WriteFile(fixture, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, persistent := range []bool{false, true} {
		for _, code := range []string{"0", "7"} {
			t.Run(code+"/"+map[bool]string{true: "persistent", false: "private"}[persistent], func(t *testing.T) {
				cacheRoot := ""
				wantRoot := root
				if persistent {
					cacheRoot = filepath.Join(root, "repository cache")
					wantRoot = cacheRoot
				}
				command := exec.CommandContext(t.Context(), "/bin/sh", launcher, fixture, "run", "--timeout=10m", "--concurrency=1", "./...")
				command.Env = append(os.Environ(), "OBERTH_CACHE_DIR="+cacheRoot, "TMPDIR="+root, "GOLANGCI_LINT_CACHE=/read-only-tools/cache", "LINT_TEST_EXIT="+code)
				output, err := command.CombinedOutput()
				if code == "0" {
					if err != nil {
						t.Fatal(err)
					}
				} else {
					var exit *exec.ExitError
					if !errors.As(err, &exit) || exit.ExitCode() != 7 {
						t.Fatalf("lint failure became %v: %s", err, output)
					}
				}
				want := filepath.Join(wantRoot, "golangci-lint-v2.12.2") + "\nrun\n--timeout=10m\n--concurrency=1\n./...\n"
				if string(output) != want {
					t.Fatalf("cache/arguments = %q, want %q", output, want)
				}
			})
		}
	}
}

func TestOberthLintLauncherKillsUncooperativeAnalysis(t *testing.T) {
	body, err := os.ReadFile(lintLauncher(t))
	if err != nil {
		t.Fatal(err)
	}
	const deadline = "exec /usr/bin/timeout --signal=TERM --kill-after=15s 12m"
	if strings.Count(string(body), deadline) != 1 {
		t.Fatal("missing hard 12-minute deadline and bounded TERM grace")
	}
	// Exercise the real launcher and Linux timeout with shortened durations.
	// The fixture ignores TERM, as an analyzer stuck outside its context can.
	launcher := filepath.Join(t.TempDir(), "lint.sh")
	body = []byte(strings.Replace(string(body), deadline, "exec /usr/bin/timeout --signal=TERM --kill-after=0.1s 0.1s", 1))
	if err := os.WriteFile(launcher, body, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), testtimeout.Start(5*time.Second))
	defer cancel()
	command := exec.CommandContext(ctx, "/bin/sh", launcher, "/bin/sh", "-c", "trap '' TERM; exec /bin/sleep 2")
	output, err := command.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("watchdog did not stop analysis: %v %s", ctx.Err(), output)
	}
	var exit *exec.ExitError
	if !errors.As(err, &exit) || (exit.ExitCode() != 124 && exit.ExitCode() != 137 && exit.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL) {
		t.Fatalf("deadline must fail, got %v: %s", err, output)
	}
}
