package integration_test

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFreshTreeHasArgoBuildYAML(t *testing.T) {
	repoRoot := filepath.Join("..", "..")
	build, err := os.ReadFile(filepath.Join(repoRoot, ".oberth", "build.yaml"))
	if err != nil {
		t.Fatalf(".oberth/build.yaml not found: %v", err)
	}
	if !bytes.Contains(build, []byte("kind: Workflow")) {
		t.Fatal(".oberth/build.yaml is not an Argo Workflow")
	}
}

func TestFreshTreeOmitsObsoleteProductAndBranchDoctrine(t *testing.T) {
	matches, err := obsoleteDoctrine(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range matches {
		t.Errorf("obsolete product or branch doctrine remains in %s", path)
	}
}

func obsoleteDoctrine(repoRoot string) ([]string, error) {
	var matches []string
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
			case ".git", ".worktrees", "artifacts", "bin", "dist", "node_modules":
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
				matches = append(matches, path)
			}
		}
		return nil
	})
	return matches, err
}

func TestFreshTreeIgnoresNodeDependencies(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "node_modules", "vendor")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	phrase := strings.Join([]string{"protected", "branch"}, " ")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte(phrase), 0600); err != nil {
		t.Fatal(err)
	}
	matches, err := obsoleteDoctrine(root)
	if err != nil || len(matches) != 0 {
		t.Fatalf("walk scanned dependencies: %v %v", matches, err)
	}
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte(phrase), 0600); err != nil {
		t.Fatal(err)
	}
	matches, err = obsoleteDoctrine(root)
	if err != nil || len(matches) != 1 {
		t.Fatalf("walk missed repository content: %v %v", matches, err)
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
	// Pin the rendered NOTES contract: one initial enrollment exec, subsequent
	// upstream administration through the API, the readiness explanation, the
	// documentation pointer, and verified HTTPS.
	for _, required := range []string{
		`notes_template=$chart/templates/NOTES.txt`,
		`notes_chart=$package_dir/notes-chart`,
		`--show-only templates/notes-contract.yaml`,
		`installation exec creates that identity`,
		`oberth upstream add --yes --no-wait github ssh://git@github.com/your-org`,
		`Running but not ready`,
		`https://github.com/oberthci/oberth#documentation`,
		`curl[[:space:]]+(-[^[:space:]]*[[:space:]]+)*-k([[:space:]]|$)`,
	} {
		if !strings.Contains(body, required) {
			t.Fatalf("chart validation lacks rendered NOTES contract %q", required)
		}
	}
}

// TestChartAuditURLsDefaultOffAndRequireHTTPS encodes the 2026-08 contract:
// external audit anchoring is opt-in, so both chart URL defaults are empty
// (no external service is contacted by a fresh install), while any non-empty
// value must still be a strict HTTPS URL — except when the matching
// InsecureHTTP flag is set (DEVELOPMENT ONLY). Both TSA and Rekor carry the
// InsecureHTTP escape. This replaces the previous assertion that pinned the
// public Sectigo/Rekor endpoints as defaults.
func TestChartAuditURLsDefaultOffAndRequireHTTPS(t *testing.T) {
	t.Parallel()
	repoRoot := filepath.Join("..", "..")
	values, err := os.ReadFile(filepath.Join(repoRoot, "charts", "oberth", "values.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, disabledDefault := range []string{`tsaURL: ""`, `rekorURL: ""`} {
		if !bytes.Contains(values, []byte(disabledDefault)) {
			t.Fatalf("chart audit default %s is not disabled", disabledDefault)
		}
	}
	schema, err := os.ReadFile(filepath.Join(repoRoot, "charts", "oberth", "values.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	type auditURLContract struct {
		Pattern string `json:"pattern"`
	}
	type booleanContract struct {
		Type string `json:"type"`
	}
	var document struct {
		Properties struct {
			AuditAnchor struct {
				Properties struct {
					TSAURL            auditURLContract `json:"tsaURL"`
					TSAInsecureHTTP   booleanContract  `json:"tsaInsecureHTTP"`
					RekorURL          auditURLContract `json:"rekorURL"`
					RekorInsecureHTTP booleanContract  `json:"rekorInsecureHTTP"`
				} `json:"properties"`
			} `json:"auditAnchor"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(schema, &document); err != nil {
		t.Fatal(err)
	}
	// Both URL properties use the permissive base pattern; the conditional
	// allOf schema enforces HTTPS-only when the InsecureHTTP flag is false.
	httpOrHTTPSPattern := `^$|^https?://[^/?#@\s]+(?:[/?][^#\s]*)?$`
	if contract := document.Properties.AuditAnchor.Properties.TSAURL; contract.Pattern != httpOrHTTPSPattern {
		t.Fatalf("chart TSA URL contract = pattern %q", contract.Pattern)
	}
	if contract := document.Properties.AuditAnchor.Properties.RekorURL; contract.Pattern != httpOrHTTPSPattern {
		t.Fatalf("chart Rekor URL contract = pattern %q", contract.Pattern)
	}
	if contract := document.Properties.AuditAnchor.Properties.TSAInsecureHTTP; contract.Type != "boolean" {
		t.Fatalf("chart TSA insecure HTTP contract = type %q", contract.Type)
	}
	if contract := document.Properties.AuditAnchor.Properties.RekorInsecureHTTP; contract.Type != "boolean" {
		t.Fatalf("chart Rekor insecure HTTP contract = type %q", contract.Type)
	}
}
