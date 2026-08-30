package pipelinegen

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/pkg/argoworkflow"
)

// materialize lays a testdata fixture out the way a real checkout is: the
// workflow under .github/workflows, the remote under .git/config, and the
// dotfiles restored to their real names.
func materialize(t *testing.T, fixture string) string {
	t.Helper()
	root := t.TempDir()
	source := filepath.Join("testdata", fixture)
	entries, err := os.ReadDir(source)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		raw, err := os.ReadFile(filepath.Join(source, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		var target string
		switch entry.Name() {
		case "build.yml":
			target = filepath.Join(root, ".github", "workflows", "build.yml")
		case "git-config":
			target = filepath.Join(root, ".git", "config")
		case "npmrc":
			target = filepath.Join(root, ".npmrc")
		case "nvmrc":
			target = filepath.Join(root, ".nvmrc")
		default:
			target = filepath.Join(root, entry.Name())
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// generateFor runs the whole path a real `oberth init` runs.
func generateFor(t *testing.T, fixture string) Result {
	t.Helper()
	root := materialize(t, fixture)
	project := DetectProject(root)
	if workflow, ok := FindBuildWorkflow(root); ok {
		Apply(workflow, &project)
	}
	// The org is the caller's to supply, the way `oberth init` supplies the
	// one the server has registered. Detection deliberately does not set it.
	project.Org = "acme"
	return Generate(project)
}

// admitGenerated is the point of this file: the generated document goes
// through the REAL gate, not a copy of its rules. A generator checked against
// a restatement of the rules drifts from the server the first time either one
// changes, and the drift shows up as a refused push.
func admitGenerated(t *testing.T, document string) {
	t.Helper()
	workflow, err := argoworkflow.Decode([]byte(document))
	if err != nil {
		t.Fatalf("the generated pipeline does not decode: %v\n\n%s", err, document)
	}
	if err := argoworkflow.Admit(workflow, argoworkflow.Policy{}); err != nil {
		t.Fatalf("the generated pipeline is refused by admission: %v\n\n%s", err, document)
	}
}

func TestGeneratedNodePipelineIsAdmitted(t *testing.T) {
	t.Parallel()
	result := generateFor(t, "node")
	admitGenerated(t, result.YAML)

	if !result.Complete {
		t.Fatal("a repository with a package.json and a build workflow must produce a finished pipeline")
	}
	for _, want := range []string{"copy-source", "install", "lint", "typecheck", "test", "build"} {
		if !containsStep(result.Steps, want) {
			t.Fatalf("step %q missing from %v", want, result.Steps)
		}
	}
	if !strings.Contains(result.YAML, "npm ci --no-audit --no-fund --legacy-peer-deps") {
		t.Fatalf("LEGACY_PEER_DEPS from the Actions inputs was not applied:\n%s", result.YAML)
	}
	if !strings.Contains(result.YAML, "node:20-trixie-slim@sha256:") {
		t.Fatalf("NODEJS_VERSION 20 from the Actions inputs did not select the Node 20 image:\n%s", result.YAML)
	}
	// npx tsc with no local typescript fetches the abandoned `tsc` package and
	// reports TS6046, which reads like a config error. The repository's own
	// script is what must run.
	if strings.Contains(result.YAML, "npx tsc") {
		t.Fatalf("generated pipeline must not invoke npx tsc:\n%s", result.YAML)
	}
	if !strings.Contains(result.YAML, "npm run typecheck") {
		t.Fatalf("typecheck must run the repository's own script:\n%s", result.YAML)
	}
}

func TestGeneratedNodePipelineDeclaresTheUpstreamTokenAndItsGrant(t *testing.T) {
	t.Parallel()
	result := generateFor(t, "node")

	if result.SecretPath != "oberth/upstream/acme/github-token" {
		t.Fatalf("SecretPath = %q", result.SecretPath)
	}
	if !strings.Contains(result.YAML, "oberth.ci/secret-paths: oberth/upstream/acme/github-token") {
		t.Fatalf("the path must be declared in the workflow annotation:\n%s", result.YAML)
	}
	// Admission checks every `oberth secretstore exec --path` against the
	// annotation, so the two spellings must be identical.
	if !strings.Contains(result.YAML, "--path=oberth/upstream/acme/github-token") {
		t.Fatalf("the exec path must match the annotation:\n%s", result.YAML)
	}
	want := "oberth access allow web-portal '*' oberth/upstream/acme/github-token"
	if result.GrantCommand != want {
		t.Fatalf("GrantCommand = %q, want %q", result.GrantCommand, want)
	}
	if !strings.Contains(result.YAML, want) {
		t.Fatalf("the grant command must be in the generated header:\n%s", result.YAML)
	}
	if !strings.Contains(result.YAML, `find "$OBERTH_SECRETSTORE_DIR"`) {
		t.Fatalf("the step must find the delivered secret:\n%s", result.YAML)
	}
}

func TestGeneratedMavenPipelineIsAdmitted(t *testing.T) {
	t.Parallel()
	result := generateFor(t, "maven")
	admitGenerated(t, result.YAML)

	if !result.Complete {
		t.Fatal("a repository with a pom.xml must produce a finished pipeline")
	}
	for _, want := range []string{"copy-source", "test", "package"} {
		if !containsStep(result.Steps, want) {
			t.Fatalf("step %q missing from %v", want, result.Steps)
		}
	}
	// JAVA_VERSION arrives as ${{ vars.JAVA_25_VERSION }}; the major is 25.
	if !strings.Contains(result.YAML, "maven:3.9-eclipse-temurin-25@sha256:") {
		t.Fatalf("JAVA_VERSION did not select the Java 25 image:\n%s", result.YAML)
	}
	// The parent is com.acme, which does not resolve from Central.
	if result.SecretPath != "oberth/upstream/acme/github-token" {
		t.Fatalf("a non-public parent must make the build credentialed, got %q", result.SecretPath)
	}
	if !strings.Contains(result.YAML, "mvn -B -ntp") {
		t.Fatalf("maven steps missing:\n%s", result.YAML)
	}
}

// TestGeneratedPipelinesRecordWhatWasNotTranslated is the honesty test. Both
// sample workflows delegate the entire build to a reusable workflow in another
// repository, so the steps here came from the manifests instead. Saying so is
// the difference between a translation and a guess.
func TestGeneratedPipelinesRecordWhatWasNotTranslated(t *testing.T) {
	t.Parallel()
	for _, fixture := range []string{"node", "maven"} {
		t.Run(fixture, func(t *testing.T) {
			t.Parallel()
			result := generateFor(t, fixture)
			if !strings.Contains(result.YAML, "What was NOT translated") {
				t.Fatalf("the header must list what was not translated:\n%s", result.YAML)
			}
			if !strings.Contains(result.YAML, "reusable-workflows") {
				t.Fatalf("the delegated build must be named:\n%s", result.YAML)
			}
			if !strings.Contains(result.YAML, "SLACK_WEBHOOK_URL") {
				t.Fatalf("the secrets Actions passed and Oberth lacks must be named:\n%s", result.YAML)
			}
		})
	}
}

// TestUnknownProjectFailsLoudlyInsteadOfPassingQuietly is the whole point of
// the rewrite: the generator this replaces emitted a demo DAG that went green
// on every repository, so a repository nobody had translated looked exactly
// like one that worked.
func TestUnknownProjectFailsLoudlyInsteadOfPassingQuietly(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	project := DetectProject(root)
	result := Generate(project)
	admitGenerated(t, result.YAML)

	if result.Complete {
		t.Fatal("an unrecognized repository must not report a finished pipeline")
	}
	if !strings.Contains(result.YAML, "THIS PIPELINE IS NOT FINISHED") {
		t.Fatalf("the scaffold must say it is unfinished:\n%s", result.YAML)
	}
	if !strings.Contains(result.YAML, "exit 1") {
		t.Fatalf("the scaffold must fail rather than pass by doing nothing:\n%s", result.YAML)
	}
	if result.SecretPath != "" {
		t.Fatalf("a scaffold must declare no secrets, got %q", result.SecretPath)
	}
}

// TestReleaseWorkflowsAreNeverTranslated guards the selection rule. Picking
// release.yml would generate a branch pipeline that deploys.
func TestReleaseWorkflowsAreNeverTranslated(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	dir := filepath.Join(root, ".github", "workflows")
	if err := os.MkdirAll(filepath.Join(dir, "archived"), 0o750); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"release.yml":        "name: Release\njobs:\n  release:\n    steps:\n    - run: ./deploy.sh\n",
		"rollback.yml":       "name: Rollback\njobs:\n  rollback:\n    steps:\n    - run: ./rollback.sh\n",
		"cache-warming.yml":  "name: Cache\njobs:\n  warm:\n    steps:\n    - run: ./warm.sh\n",
		"archived/build.yml": "name: Old Build\njobs:\n  build:\n    steps:\n    - run: ./old.sh\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if workflow, ok := FindBuildWorkflow(root); ok {
		t.Fatalf("selected %q, but none of these is a build workflow", workflow.Path)
	}
}

func containsStep(steps []string, want string) bool {
	for _, step := range steps {
		if step == want {
			return true
		}
	}
	return false
}

// The org is registered on the server and nowhere else. A checkout cloned from
// a local path names its containing directory here, and a secret path built
// from that word is refused at admission -- so detection must not produce one
// at all, rather than produce a plausible wrong one.
func TestDetectionDoesNotInventAnOrg(t *testing.T) {
	t.Parallel()
	project := DetectProject(materialize(t, "node"))
	if project.Org != "" {
		t.Fatalf("detection guessed an org: %q", project.Org)
	}
	if project.OriginOrg == "" {
		t.Fatal("the origin's org was not kept for the disagreement note")
	}
}

// A disagreement is information, not a failure: the file says which one is
// enforced rather than silently using either.
func TestADisagreementWithTheOriginIsSaidInTheFile(t *testing.T) {
	t.Parallel()
	project := DetectProject(materialize(t, "node"))
	project.Org = "acme"
	document := Generate(project).YAML
	if !strings.Contains(document, "acme") || !strings.Contains(document, project.OriginOrg) {
		t.Fatalf("the header does not name both orgs:\n%s", document)
	}
}
