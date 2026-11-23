package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestStaticEntrypointsRunTheSameThreeChecks(t *testing.T) {
	for _, entry := range []string{"make", "script"} {
		t.Run(entry, func(t *testing.T) {
			dir := workflowFixture(t)
			log := filepath.Join(dir, "static.log")
			writeFixtureCommand(t, filepath.Join(dir, "bin/git"), "exit 0\n")
			writeFixtureCommand(t, filepath.Join(dir, "bin/go"), `case "$*" in
*'version --short') cat hack/lint-version | sed 's/^v//';;
*'run '*./...) echo lint >> "$STATIC_LOG";;
*) exit 98;; esac
`)
			writeFixtureCommand(t, filepath.Join(dir, "bin/python3"), `case "$*" in
'hack/guards/run.py') echo guards >> "$STATIC_LOG";;
'hack/plans/parity.py --check') echo parity >> "$STATIC_LOG";;
*) exit 98;; esac
`)
			delivery, _ := staticPolicyFile(t, "present")
			command := exec.CommandContext(t.Context(), "make", "--no-print-directory", "static")
			if entry == "script" {
				command = exec.CommandContext(t.Context(), "bash", "hack/static.sh")
			}
			command.Dir = dir
			command.Env = append(os.Environ(), "PATH="+filepath.Join(dir, "bin")+":"+os.Getenv("PATH"),
				"STATIC_LOG="+log, "OBERTH_SECRETS_DIR="+delivery, "BUILD_DEFAULT_REF=main",
				"GOLANGCI_LINT="+filepath.Join(dir, "hack/lint-tool.sh"))
			if output, err := command.CombinedOutput(); err != nil {
				t.Fatal("static entry point failed", err, string(output))
			}
			body, err := os.ReadFile(log)
			if err != nil || string(body) != "lint\nguards\nparity\n" {
				t.Fatal("static entry point checks differ", err, string(body))
			}
		})
	}
}
