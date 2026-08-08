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

func (*GoSDK) Build(pkg string) Step {
	step := Step{Name: sdkStepName("go-build", pkg), Command: "go", Args: []string{"build", pkg}, Env: map[string]string{"CGO_ENABLED": "0"}}
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

// ScanSDK defines common offline scan steps.
type ScanSDK struct{}

func (*ScanSDK) TrivyFS(path string) Step {
	step := Step{
		Name: sdkStepName("trivy-fs", path), Command: "trivy",
		Args: []string{
			"fs", "--cache-dir", "/seed/trivy", "--cache-backend", "memory",
			"--offline-scan", "--skip-db-update", "--skip-java-db-update",
			"--skip-check-update", "--skip-vex-repo-update", "--skip-version-check",
			"--disable-telemetry", "--exit-code", "1", "--severity", "HIGH,CRITICAL",
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
