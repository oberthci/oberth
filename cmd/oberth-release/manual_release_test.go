package main

import (
	"fmt"
	"helm.sh/helm/v3/pkg/chart/loader"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestReleaseScriptDirectoryRefusalsNameWritableRecovery(t *testing.T) {
	for _, variable := range []string{"RELEASE_PRIVATE", "RELEASE_SHARED", "RELEASE_VERIFY_DIR"} {
		t.Run(variable, func(t *testing.T) {
			env := releaseEnvironment(t)
			path := filepath.Join(t.TempDir(), "file")
			if err := os.WriteFile(path, []byte("occupied"), 0600); err != nil {
				t.Fatal(err)
			}
			env[variable] = path
			output, err := releaseScript(t, "verify", env)
			want := "Release directory is unusable; set the named path to a writable directory.\nDirectory: " + variable + "=" + path + "\n"
			if err == nil || string(output) != want {
				t.Fatal("directory refusal omitted its whole recovery", err, string(output))
			}
		})
	}
}

func TestReleaseScriptMissingGitHubTokenRefusesBeforeEffects(t *testing.T) {
	for _, operation := range []string{"publish", "finalize"} {
		env := releaseEnvironment(t)
		env["OBERTH_SECRETS_DIR"] = t.TempDir()
		output, err := releaseScript(t, operation, env)
		want := "GitHub release token is missing; grant it to this step.\n"
		if err == nil || string(output) != want {
			t.Fatal("missing token lacks a release refusal", err, string(output))
		}
		if _, err := os.Stat(env["RELEASE_PRIVATE"]); !os.IsNotExist(err) {
			t.Fatal("missing token reached side effects", err)
		}
	}
}

func TestManualReleaseProcedureIsOrderedAndShellValid(t *testing.T) {
	body, err := os.ReadFile("../../docs/releasing.md")
	if err != nil {
		t.Fatal(err)
	}
	_, section, ok := strings.Cut(string(body), "## First release by hand\n")
	if !ok {
		t.Fatal("manual release procedure is missing")
	}
	section, _, _ = strings.Cut(section, "\n## ")
	for _, required := range []string{releaseGoVersion(t), "GOTOOLCHAIN=local", "RELEASE_PRIVATE", "RELEASE_SHARED", "RELEASE_VERIFY_DIR", "OBERTH_SECRETS_DIR", "RELEASE_CACHE_A", "RELEASE_CACHE_B", "--token-file"} {
		if !strings.Contains(section, required) {
			t.Fatal("manual procedure omits", required)
		}
	}
	remaining := section
	for _, step := range []string{"generate-key-pair", "sh hack/release-pin.sh", "git commit", "base publication procedure", "bash hack/prepare-base.sh", "make release-bytes", "go build", "bash hack/release.sh sign", `"$RELEASE_TOOL" verify`, `"$RELEASE_TOOL" publish`, "bash hack/release.sh verify", `"$RELEASE_TOOL" finalize`} {
		_, after, ok := strings.Cut(remaining, step)
		if !ok {
			t.Fatal("manual release step is missing or out of order", step)
		}
		remaining = after
	}
	var script strings.Builder
	for _, block := range strings.Split(section, "```sh\n")[1:] {
		commands, _, _ := strings.Cut(block, "```")
		script.WriteString(commands)
	}
	command := exec.CommandContext(t.Context(), "bash", "-n")
	command.Stdin = strings.NewReader(script.String())
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatal("manual commands have invalid shell syntax", err, string(output))
	}
}

func TestReleaseBuildMissingCachesRefusesBeforeCompilation(t *testing.T) {
	dir := releaseBuildTree(t)
	output, err := buildScript(t, dir, map[string]string{"RELEASE_CACHE_A": "", "RELEASE_CACHE_B": ""})
	if err == nil || string(output) != "Release caches are missing; set RELEASE_CACHE_A and RELEASE_CACHE_B.\n" {
		t.Fatal("missing caches lack whole guidance", err, string(output))
	}
	if _, err := os.Stat(filepath.Join(dir, "build.log")); !os.IsNotExist(err) {
		t.Fatal("missing caches reached compilation", err)
	}
}

func TestReleaseGuideStatesPinnedKeyAndPublisherTrust(t *testing.T) {
	body, err := os.ReadFile("../../docs/releasing.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{"scripts refuse other addresses.", "Signatures are checked only against the committed pinned key; no public log is used."} {
		if !strings.Contains(string(body), statement) {
			t.Fatal("release trust statement missing", statement)
		}
	}
}

func TestManualReleaseGuideCoversTheWholeHandoffWithinBudget(t *testing.T) {
	body, err := os.ReadFile("../../docs/releasing.md")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(body), "\n") > 247 {
		t.Fatal("release guide grew beyond its existing line budget")
	}
	text := strings.Join(strings.Fields(string(body)), " ")
	for _, fact := range []string{
		"built commit must be that public pinned commit", "scripts create no tag",
		"strictly above", "12 GiB", "eight minutes", "0.7 GiB", "$TMPDIR",
		"404 means no floor", "crane auth login europe-west4-docker.pkg.dev", "--password-stdin", "make, grep and",
		`grep -E '"commit"|sha256:' "$RELEASE_DIR/release.json"`,
		"prepare-signing", "sign offline", "Release bytes built.", "Release signature verified.",
		"Release verified.", "file count and destination", "Finalize prints where `latest` points",
		"exit 0 is intentional", "encrypted offline key custody", "delete both plaintext copies",
		`"$RELEASE_WORK/release.key" "$OBERTH_SECRETS_DIR/oberth/data/release/signing"`,
	} {
		if !strings.Contains(text, fact) {
			t.Error("manual release guide omits", fact)
		}
	}
	chart, err := loader.LoadDir("../../charts/oberth")
	if err != nil {
		t.Fatal(err)
	}
	for _, pair := range []struct{ image, value string }{
		{"oberth", "imageRepository"}, {"oberth-source-plugin", "sourcePluginImageRepository"},
	} {
		command := fmt.Sprintf(`go tool -modfile=hack/tools/crane.mod crane push "$RELEASE_DIR/%s.oci" \ "%s:$VERSION"`, pair.image, chart.Values[pair.value])
		if !strings.Contains(text, command) {
			t.Error("guide push differs from the chart repository", command)
		}
	}
}
