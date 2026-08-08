package periapsis

import (
	"fmt"
	"strings"
)

// Context is passed to the interpreted Pipeline function.
type Context struct {
	Repo   string
	Branch string
	SHA    string
	Tag    string

	Go   *GoSDK
	Lint *LintSDK
	Scan *ScanSDK
}

// NewContext constructs a context with every typed SDK initialized.
func NewContext(repo, branch, sha, tag string) *Context {
	return &Context{
		Repo: repo, Branch: branch, SHA: sha, Tag: tag,
		Go: &GoSDK{}, Lint: &LintSDK{}, Scan: &ScanSDK{},
	}
}

// GoSDK defines common Go tool steps.
type GoSDK struct{}

// Build compiles pkg for the runner's native platform. It never sets GOOS or
// GOARCH, so two burns that both call Build produce byte-identical native
// binaries regardless of their names — for cross-compilation use BuildFor.
func (*GoSDK) Build(pkg string) Step {
	step := Step{Name: sdkStepName("go-build", pkg), Command: "go", Args: []string{"build", pkg}, Env: map[string]string{"CGO_ENABLED": "0"}}
	return step.withArgInsertionPoint(len(step.Args) - 1)
}

// BuildFor cross-compiles pkg for an explicit target platform. The target is
// part of the step definition itself, so a burn named for one platform cannot
// silently produce a native build: green cross-compile evidence always comes
// from a step whose environment pins GOOS and GOARCH.
func (*GoSDK) BuildFor(goos, goarch, pkg string) Step {
	step := Step{
		Name:    sdkStepName("go-build-"+goos+"-"+goarch, pkg),
		Command: "go", Args: []string{"build", pkg},
		Env: map[string]string{"CGO_ENABLED": "0", "GOOS": goos, "GOARCH": goarch},
	}
	return step.withArgInsertionPoint(len(step.Args) - 1)
}

func (*GoSDK) Test(pkg string) Step {
	step := Step{Name: sdkStepName("go-test", pkg), Command: "go", Args: []string{"test", "-v", "-count=1", pkg}}
	return step.withArgInsertionPoint(len(step.Args) - 1)
}

func (*GoSDK) TestRace(pkg string) Step {
	step := Step{Name: sdkStepName("go-test-race", pkg), Command: "go", Args: []string{"test", "-race", "-v", "-count=1", pkg}, Env: map[string]string{"CGO_ENABLED": "1"}}
	return step.withArgInsertionPoint(len(step.Args) - 1)
}

func (*GoSDK) Vet(pkg string) Step {
	step := Step{Name: sdkStepName("go-vet", pkg), Command: "go", Args: []string{"vet", pkg}}
	return step.withArgInsertionPoint(len(step.Args) - 1)
}

// LintSDK defines common lint steps.
type LintSDK struct{}

func (*LintSDK) GolangciLint(pkg string) Step {
	step := Step{
		Name: sdkStepName("golangci-lint", pkg), Command: "golangci-lint",
		Args: []string{"run", "--timeout=10m", "--concurrency=1", pkg},
		Env:  map[string]string{"GOMEMLIMIT": "768MiB"},
	}
	return step.withArgInsertionPoint(len(step.Args) - 1)
}

// ScanSDK defines common repository scan steps. Scanner binaries and databases
// are installed by repository-owned setup burns, not baked into the runner.
type ScanSDK struct{}

func (*ScanSDK) TrivyFS(path string) Step {
	step := Step{
		Name: sdkStepName("trivy-fs", path), Command: "trivy",
		Args: []string{
			"fs", "--cache-dir", "/tmp/oberth-trivy",
			"--skip-check-update", "--skip-vex-repo-update", "--skip-version-check",
			"--disable-telemetry", "--scanners", "vuln",
			"--exit-code", "1", "--severity", "HIGH,CRITICAL",
			"--skip-dirs", ".git", path,
		},
	}
	return step.withArgInsertionPoint(len(step.Args) - 1)
}

func sdkStepName(prefix, value string) string {
	suffix := sanitizeName(value)
	limit := maxDefinitionNameBytes - len(prefix) - 1
	if len(suffix) > limit {
		suffix = suffix[:limit]
	}
	return fmt.Sprintf("%s-%s", prefix, suffix)
}

func sanitizeName(value string) string {
	var out strings.Builder
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			out.WriteRune(r)
		default:
			out.WriteByte('-')
		}
	}
	if out.Len() == 0 {
		return "value"
	}
	return out.String()
}
