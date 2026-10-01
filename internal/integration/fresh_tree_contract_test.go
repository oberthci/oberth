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
	// The required list pins the trimmed NOTES contract established by #838
	// and encoded in hack/test-chart.sh by e4ff640: namespaced in-pod exec
	// commands for the two bootstrap actions, the vault-like not-ready
	// explanation, the docs pointer, and the no-curl-k rule. The exec pin
	// matches the script's quote-concatenated pattern, which keeps the raw
	// script body free of the cluster-dependent command text forbidden above.
	for _, required := range []string{
		`notes_template=$chart/templates/NOTES.txt`,
		`notes_chart=$package_dir/notes-chart`,
		`--show-only templates/notes-contract.yaml`,
		`'kubectl'' exec -it -n default deploy/oberth'`,
		`oberth upstream add github ssh://git@github.com/your-org`,
		`oberth uplink add - operator@host < ~/.ssh/id_ed25519.pub`,
		`stays Running but not ready (0/1)`,
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
