package integration_test

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestReleaseBuildTargetsCoveredByBranchCI ensures that every GOOS/GOARCH
// target cross-compiled by .oberth/release.sh build_artifacts has a
// corresponding build-<os>-<arch> step in .oberth/build.yaml. A branch CI
// that compiles fewer targets than the release lets platform-specific type
// errors (like syscall.Statfs_t.Bsize int64 vs uint32 on darwin) through
// nine green branch runs only to burn at release time.
//
// Issue #699: v0.16.16 release burned at release-build because branch CI
// compiled linux only while release.sh cross-compiles darwin/{amd64,arm64}.
func TestReleaseBuildTargetsCoveredByBranchCI(t *testing.T) {
	repoRoot := filepath.Join("..", "..")
	releaseTargets := parseReleaseTargets(t, filepath.Join(repoRoot, ".oberth", "release.sh"))
	buildTargets := parseBuildYAMLTargets(t, filepath.Join(repoRoot, ".oberth", "build.yaml"))

	if len(releaseTargets) == 0 {
		t.Fatal("no release targets found in release.sh build_artifacts")
	}
	if len(buildTargets) == 0 {
		t.Fatal("no build targets found in build.yaml")
	}

	// Every release target must have a matching branch CI build step.
	for _, target := range releaseTargets {
		if _, ok := buildTargets[target]; !ok {
			t.Errorf("release.sh builds %s but build.yaml has no build step for it", target)
		}
	}

	// Report any build.yaml targets not in release.sh (informational, not fatal).
	for target := range buildTargets {
		found := false
		for _, rt := range releaseTargets {
			if rt == target {
				found = true
				break
			}
		}
		if !found {
			t.Logf("build.yaml has build step for %s which is not a release target (informational)", target)
		}
	}
}

// parseReleaseTargets extracts the GOOS/GOARCH pairs from the
// "for target in ..." loop inside release.sh's build_artifacts function.
// The line has the form: for target in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64; do
func parseReleaseTargets(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading release.sh: %v", err)
	}

	// Match the "for target in ... ; do" line inside build_artifacts.
	re := regexp.MustCompile(`for\s+target\s+in\s+((?:[a-z0-9]+/[a-z0-9]+\s*)+);\s*do`)
	matches := re.FindAllStringSubmatch(string(data), -1)
	if len(matches) == 0 {
		t.Fatal("could not find 'for target in ...; do' in release.sh")
	}

	// Use the first match (build_artifacts has the canonical target list).
	fields := strings.Fields(matches[0][1])
	var targets []string
	for _, f := range fields {
		parts := strings.SplitN(f, "/", 2)
		if len(parts) != 2 {
			t.Fatalf("malformed target %q in release.sh", f)
		}
		targets = append(targets, f)
	}
	return targets
}

// parseBuildYAMLTargets extracts the GOOS/GOARCH from build-* template
// definitions in build.yaml by scanning for templates whose name starts with
// "build-" and reading their GOOS and GOARCH env values. When GOOS is not
// explicitly set, it defaults to "linux" because all Argo containers run on
// linux nodes.
func parseBuildYAMLTargets(t *testing.T, path string) map[string]bool {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("reading build.yaml: %v", err)
	}
	defer f.Close()

	targets := make(map[string]bool)
	scanner := bufio.NewScanner(f)

	// State machine: when we see "- name: build-*" as a template definition
	// (top-level, not a DAG task), collect GOOS and GOARCH from its env block.
	var inBuildTemplate bool
	var templateName string
	var goos, goarch string

	flush := func() {
		if !inBuildTemplate || goarch == "" {
			return
		}
		effectiveGOOS := goos
		if effectiveGOOS == "" {
			// The container image is linux; GOOS defaults to linux.
			effectiveGOOS = "linux"
		}
		targets[fmt.Sprintf("%s/%s", effectiveGOOS, goarch)] = true
	}

	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)

		// Template definition: "  - name: build-*" at exactly 2-space indent
		// (Argo template list items).
		if strings.HasPrefix(line, "  - name: build-") && !strings.HasPrefix(line, "      ") {
			// Flush previous template.
			flush()
			templateName = strings.TrimPrefix(trimmed, "- name: ")
			inBuildTemplate = true
			goos = ""
			goarch = ""
			continue
		}

		// Another template definition resets.
		if strings.HasPrefix(line, "  - name: ") && !strings.HasPrefix(line, "      ") && !strings.HasPrefix(trimmed, "- name: build-") {
			flush()
			inBuildTemplate = false
			templateName = ""
			goos = ""
			goarch = ""
			continue
		}

		if !inBuildTemplate {
			continue
		}

		// Inside a build template, look for GOOS and GOARCH env entries.
		// Pattern: "      - name: GOOS" followed by "        value: ..."
		if trimmed == "- name: GOOS" {
			if scanner.Scan() {
				valLine := strings.TrimSpace(scanner.Text())
				if strings.HasPrefix(valLine, "value:") {
					goos = strings.Trim(strings.TrimPrefix(valLine, "value:"), " \"")
				}
			}
		}
		if trimmed == "- name: GOARCH" {
			if scanner.Scan() {
				valLine := strings.TrimSpace(scanner.Text())
				if strings.HasPrefix(valLine, "value:") {
					goarch = strings.Trim(strings.TrimPrefix(valLine, "value:"), " \"")
				}
			}
		}

		_ = templateName // used for debugging
	}
	// Flush last template.
	flush()

	if err := scanner.Err(); err != nil {
		t.Fatalf("scanning build.yaml: %v", err)
	}
	return targets
}

// TestReleaseBuildTargetsNoDuplicates ensures the release target list has no
// duplicate entries, which would silently double-build the same binary.
func TestReleaseBuildTargetsNoDuplicates(t *testing.T) {
	repoRoot := filepath.Join("..", "..")
	targets := parseReleaseTargets(t, filepath.Join(repoRoot, ".oberth", "release.sh"))
	seen := make(map[string]bool)
	for _, target := range targets {
		if seen[target] {
			t.Errorf("release.sh lists duplicate target %s", target)
		}
		seen[target] = true
	}
}
