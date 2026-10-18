package installer

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// Every oberth chart value the installer passes with --set must be in
// installerManagedValues (or applied by restore itself); otherwise a restore
// would tell operators to re-apply a stale, install-specific value.
func TestInstallerManagedValuesCoverEverySet(t *testing.T) {
	t.Parallel()
	topLevel := map[string]bool{}
	values, err := os.Open(filepath.Join("..", "..", "charts", "oberth", "values.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = values.Close() }()
	scanner := bufio.NewScanner(values)
	for scanner.Scan() {
		if line := scanner.Text(); line != "" && line[0] != ' ' && line[0] != '#' {
			if key, _, ok := strings.Cut(line, ":"); ok {
				topLevel[key] = true
			}
		}
	}
	setFlag := regexp.MustCompile("\"--set(?:-string)?\",\\s*(?:fmt\\.Sprintf\\()?[`\"]([A-Za-z][A-Za-z0-9.\\\\/-]*)")
	sources, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, source := range sources {
		if strings.HasSuffix(source, "_test.go") {
			continue
		}
		body, err := os.ReadFile(source) // #nosec G304 -- this package's own sources.
		if err != nil {
			t.Fatal(err)
		}
		for _, match := range setFlag.FindAllStringSubmatch(string(body), -1) {
			key := strings.ReplaceAll(match[1], `\.`, ".") // escaped dots in a --set key
			if top, _, _ := strings.Cut(key, "."); !topLevel[top] {
				continue // another chart's value (OpenBao, Argo Workflows)
			}
			checked++
			if !installerManagedValue(key) && !slices.ContainsFunc(restoreAppliedValues, func(applied string) bool {
				return key == applied || strings.HasPrefix(key, applied+".")
			}) {
				t.Errorf("%s sets %s, which installerManagedValues does not list", source, key)
			}
		}
	}
	if checked < 20 {
		t.Fatalf("found only %d oberth --set keys; the pattern no longer matches the installer", checked)
	}
}
