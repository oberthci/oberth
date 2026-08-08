package integration_test

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/pkg/periapsis"
)

func TestFreshTreeUsesOnlyPeriapsisCI(t *testing.T) {
	repoRoot := filepath.Join("..", "..")
	if _, err := os.Stat(filepath.Join(repoRoot, ".oberth", "ci.yaml")); !os.IsNotExist(err) {
		t.Fatalf("legacy CI definition still exists: %v", err)
	}
	source, err := os.ReadFile(filepath.Join(repoRoot, ".oberth", "periapsis.go"))
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{
		strings.Join([]string{"argoproj", "io"}, "."),
		strings.Join([]string{"kind", "Workflow"}, ": "),
		strings.Join([]string{"", "opt", "trivy-cache"}, "/"),
	} {
		if bytes.Contains(source, []byte(forbidden)) {
			t.Fatalf("fresh Periapsis source retains legacy contract %q", forbidden)
		}
	}
	pipeline, err := periapsis.Interpret(string(source), periapsis.NewContext("oberth", "main", strings.Repeat("a", 40), ""))
	if err != nil {
		t.Fatalf("interpret fresh Periapsis source: %v", err)
	}
	selected, err := periapsis.Select(*pipeline, periapsis.TriggerCI)
	if err != nil {
		t.Fatalf("select CI burns: %v", err)
	}
	assertGoPythonSetupStep(t, selected, "setup")
	assertRepositoryEnvironment(t, selected)
	assertInstallerDownloadCache(t, selected, 5)
	assertBurnStepNames(t, selected, "build-amd64", []string{"build-oberth-amd64", "build-oberth-runner-amd64"})
	assertBurnStepNames(t, selected, "build-arm64", []string{"build-oberth-arm64", "build-oberth-runner-arm64"})
}

func assertBurnStepNames(t *testing.T, pipeline periapsis.Pipeline, burnName string, want []string) {
	t.Helper()
	for _, burn := range pipeline.Burns {
		if burn.Name != burnName {
			continue
		}
		got := make([]string, 0, len(burn.Steps))
		for _, step := range burn.Steps {
			got = append(got, step.Name)
		}
		if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
			t.Fatalf("burn %s steps = %v, want %v", burnName, got, want)
		}
		return
	}
	t.Fatalf("burn %s is missing", burnName)
}

func TestFreshTreeOmitsObsoleteProductAndBranchDoctrine(t *testing.T) {
	repoRoot := filepath.Join("..", "..")
	forbidden := [][]byte{
		[]byte(strings.Join([]string{"oberth", "lite"}, "-")),
		[]byte(strings.Join([]string{"protected", "branch"}, " ")),
		[]byte(strings.Join([]string{"protected", "branch"}, "-")),
		[]byte(strings.Join([]string{"default branch changes", "require promote"}, " ")),
	}
	err := filepath.WalkDir(repoRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			switch entry.Name() {
			case ".git", ".worktrees", "artifacts", "bin", "dist":
				return filepath.SkipDir
			}
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, phrase := range forbidden {
			if bytes.Contains(bytes.ToLower(body), phrase) {
				t.Errorf("obsolete product or branch doctrine remains in %s", path)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestChartValidationIsClusterIndependent(t *testing.T) {
	t.Parallel()
	repoRoot := filepath.Join("..", "..")
	script, err := os.ReadFile(filepath.Join(repoRoot, "hack", "test-chart.sh"))
	if err != nil {
		t.Fatal(err)
	}
	body := string(script)
	for _, forbidden := range []string{
		"helm install",
		"helm upgrade",
		"helm uninstall",
		"helm test",
		"helm status",
		"kubectl ",
	} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("chart validation retains cluster-dependent command %q", forbidden)
		}
	}
	for _, required := range []string{
		`notes_template=$chart/templates/NOTES.txt`,
		`notes_chart=$package_dir/notes-chart`,
		`--show-only templates/notes-contract.yaml`,
		`--namespace="default"`,
		`--upstream-key-secret="oberth-upstream-key"`,
		`"github" "ssh://git@github.com/your-org"`,
		`oberth uplink add - operator@host < ~/.ssh/id_ed25519.pub`,
		`--cacert "$OBERTH_TLS_CERT"`,
		`--resolve "oberth:${OBERTH_HTTPS_PORT}:${OBERTH_NODE_IP}"`,
		`Authorization: Bearer %s`,
		`"method":"initialize"`,
		`"method":"tools/list"`,
	} {
		if !strings.Contains(body, required) {
			t.Fatalf("chart validation lacks rendered NOTES contract %q", required)
		}
	}
}

func TestChartAuditTSARequiresHTTPS(t *testing.T) {
	t.Parallel()
	repoRoot := filepath.Join("..", "..")
	values, err := os.ReadFile(filepath.Join(repoRoot, "charts", "oberth", "values.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(values, []byte("tsaURL: https://timestamp.sectigo.com/rfc3161")) {
		t.Fatalf("chart TSA default is not the expected HTTPS endpoint")
	}
	schema, err := os.ReadFile(filepath.Join(repoRoot, "charts", "oberth", "values.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	type auditURLContract struct {
		Format  string `json:"format"`
		Pattern string `json:"pattern"`
	}
	var document struct {
		Properties struct {
			AuditAnchor struct {
				Properties struct {
					TSAURL   auditURLContract `json:"tsaURL"`
					RekorURL auditURLContract `json:"rekorURL"`
				} `json:"properties"`
			} `json:"auditAnchor"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(schema, &document); err != nil {
		t.Fatal(err)
	}
	for name, contract := range map[string]auditURLContract{
		"TSA":   document.Properties.AuditAnchor.Properties.TSAURL,
		"Rekor": document.Properties.AuditAnchor.Properties.RekorURL,
	} {
		if contract.Format != "uri" || contract.Pattern != `^https://[^/?#@\s]+(?:[/?][^#\s]*)?$` {
			t.Fatalf("chart %s URL contract = format %q pattern %q", name, contract.Format, contract.Pattern)
		}
	}
}
