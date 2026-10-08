package argojob

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"

	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/periapsis"
)

// This suite holds Oberth's own .oberth/build.yaml and .oberth/release.yaml to
// the engine's real admission chain. They are the first production documents
// written against this format, so they are also its first honest test: a rule
// that is too strict to express Oberth's own pipeline is a rule that is wrong.

func repositoryRoot(t *testing.T) string {
	t.Helper()
	// internal/argojob -> repository root
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolve repository root: %v", err)
	}
	return root
}

func loadPipeline(t *testing.T, name string) []byte {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(repositoryRoot(t), ".oberth", name))
	if err != nil {
		t.Fatalf("read .oberth/%s: %v", name, err)
	}
	return body
}

func oberthConfig() Config {
	config := testConfig()
	// Oberth's own pipeline uses golang: for toolchain steps and aquasec/trivy:
	// for the security scan (the official image, pinned by digest).
	config.RunnerImagePrefixes = []string{"golang:", "aquasec/trivy:"}
	return config
}

// oberthReleaseApprovedSecrets returns the approval-table grants for
// Oberth's own release pipeline. These are the paths release.yaml declares
// in its oberth.ci/secret-paths annotation.
func oberthReleaseApprovedSecrets() map[string]bool {
	return map[string]bool{
		"oberth/data/release/gar-image-key":    true,
		"oberth/data/release/gar-chart-key":    true,
		"oberth/data/release/gar-reader-key":   true,
		"oberth/data/release/r2-upload-token":  true,
		"oberth/data/release/cosign-secret":    true,
		"oberth/data/release/homebrew-tap-key": true,
		// The oberth.ci Worker deploy token, used only by release-website (#649).
		"oberth/data/release/cloudflare-oberth-workers-token": true,
	}
}

// TestOberthBuildPipelineIsAdmissible proves the CI document decodes, clears
// the structural gate, and produces a submission bound to the CI identity.
func TestOberthBuildPipelineIsAdmissible(t *testing.T) {
	built, err := Build(oberthConfig(), Request{
		RunID: "run-abc123", Name: "oberth-oberth-run-abc1-aabbccddeeff",
		Repo: "oberth", UpstreamOrg: "skipops",
		Ref: "refs/heads/main", SHA: testSHA, Trigger: periapsis.TriggerCI,
		Source:          loadPipeline(t, "build.yaml"),
		ApprovedSecrets: map[string]bool{},
	})
	if err != nil {
		t.Fatalf("Oberth's own build.yaml did not clear admission: %v", err)
	}
	if built.Spec.ServiceAccountName != testPipelineAcct {
		t.Fatalf("ServiceAccount = %q, want %q", built.Spec.ServiceAccountName, testPipelineAcct)
	}
	if built.Spec.AutomountServiceAccountToken == nil || *built.Spec.AutomountServiceAccountToken {
		t.Fatal("the CI pipeline automounts a ServiceAccount token")
	}

	// Declared weight matches periapsis.go's JobSizes{"ci": "L"}.
	size, err := argoworkflow.DeclaredSize(built)
	if err != nil || size != periapsis.L {
		t.Fatalf("declared size = %q,%v; want L", size, err)
	}

	// Every burn build.yaml declares for the CI trigger is present, with the
	// same dependency edges.
	assertBurnGraph(t, built, "ci", map[string]string{
		"setup":              "",
		"lint":               "setup",
		"test":               "lint",
		"test-openbao":       "setup",
		"security":           "setup",
		"shellcheck":         "setup",
		"chart":              "lint",
		"release-diagnostic": "setup",
		"build-amd64":        "test && chart",
		"build-arm64":        "test && chart",
		"build-darwin-amd64": "test && chart",
		"build-darwin-arm64": "test && chart",
	})
}

// TestOberthReleasePipelineIsAdmissible proves the release document clears the
// same gate and binds the release identity, and that its declared secret paths
// are the ones periapsis.go declares.
func TestOberthReleasePipelineIsAdmissible(t *testing.T) {
	built, err := Build(oberthConfig(), Request{
		RunID: "run-abc123", Name: "oberth-oberth-run-abc1-aabbccddeeff",
		Repo: "oberth", UpstreamOrg: "skipops",
		Ref: "refs/tags/v0.1.0", SHA: testSHA, Trigger: periapsis.TriggerRelease,
		Source:          loadPipeline(t, "release.yaml"),
		ApprovedSecrets: oberthReleaseApprovedSecrets(),
	})
	if err != nil {
		t.Fatalf("Oberth's own release.yaml did not clear admission: %v", err)
	}
	if built.Spec.ServiceAccountName != testCredentialedAcct {
		t.Fatalf("ServiceAccount = %q, want %q", built.Spec.ServiceAccountName, testCredentialedAcct)
	}
	if built.Spec.AutomountServiceAccountToken == nil || !*built.Spec.AutomountServiceAccountToken {
		t.Fatal("the release pipeline does not automount a ServiceAccount token")
	}

	paths, err := argoworkflow.DeclaredSecretPaths(built)
	if err != nil {
		t.Fatalf("declared secret paths: %v", err)
	}
	want := []string{
		"oberth/data/release/gar-image-key",
		"oberth/data/release/gar-chart-key",
		"oberth/data/release/gar-reader-key",
		"oberth/data/release/cosign-secret",
		"oberth/data/release/homebrew-tap-key",
		"oberth/data/release/cloudflare-oberth-workers-token",
	}
	if strings.Join(paths, ",") != strings.Join(want, ",") {
		t.Fatalf("declared paths = %v, want %v", paths, want)
	}

	assertBurnGraph(t, built, "release", map[string]string{
		"release-setup":                  "",
		"release-lint":                   "release-setup",
		"release-scan":                   "release-setup",
		"release-test":                   "release-lint && release-scan",
		"release-chart-test":             "release-setup",
		"release-build":                  "release-test && release-chart-test && release-verify-publisher-tools",
		"release-verify-publisher-tools": "release-setup",
		"release-sign-binaries":          "release-build",
		"release-publish-images":         "release-build",
		"release-publish-homebrew":       "release-publish-public",
		"release-package-chart":          "release-publish-images",
		"release-publish-chart":          "release-sign-binaries && release-package-chart",
		// TEMPORARY: release-verify absorbs homebrew's outcome via
		// status functions because the homebrew-tap-key secret is not
		// yet seeded in OpenBao. Restore to just
		// "release-publish-chart && release-publish-homebrew" once
		// the secret is seeded and the step passes.
		"release-publish-public": "release-publish-chart",
		"release-verify":         "release-publish-public && (release-publish-homebrew.Succeeded || release-publish-homebrew.Failed || release-publish-homebrew.Errored)",
		"release-runtime":        "release-verify",
		"release-finalize":       "release-runtime",
		// #649: the oberth.ci Worker publishes after the signed artifacts
		// verified; a tokenless leaf then reads the public bytes back.
		"release-website":          "release-finalize",
		"release-website-readback": "release-website",
	})
}

// TestOberthReleasePipelineOnBranchPushRefusesSystemPaths proves CI submissions
// of the release document are refused because the release document declares
// system-namespace paths (oberth/data/release/...) which are CI-ineligible.
// This is the trust-tier boundary: a branch push must never reach release
// credentials, regardless of how the document was authored.
func TestOberthReleasePipelineOnBranchPushRefusesSystemPaths(t *testing.T) {
	_, err := Build(oberthConfig(), Request{
		RunID: "run-abc123", Name: "oberth-oberth-run-abc1-aabbccddeeff",
		Repo: "oberth", UpstreamOrg: "skipops",
		Ref: "refs/heads/main", SHA: testSHA, Trigger: periapsis.TriggerCI,
		Source:          loadPipeline(t, "release.yaml"),
		ApprovedSecrets: oberthReleaseApprovedSecrets(),
	})
	if err == nil {
		t.Fatal("release.yaml with system-namespace paths was admitted for CI; CI pipelines may only declare upstream-scoped paths")
	}
	if !strings.Contains(err.Error(), "system-namespace path") {
		t.Fatalf("refusal for the wrong reason: %v", err)
	}
}

// TestOberthReleasePipelineOnBranchRefusedWithoutAllowlist proves a CI
// submission of the release document is refused because the document declares
// system-namespace paths, which are CI-ineligible regardless of the allowlist.
func TestOberthReleasePipelineOnBranchRefusedWithoutAllowlist(t *testing.T) {
	// No approval-table grants: the release document's system paths are
	// CI-ineligible regardless.
	_, err := Build(oberthConfig(), Request{
		RunID: "run-abc123", Name: "oberth-oberth-run-abc1-aabbccddeeff",
		Repo: "oberth", UpstreamOrg: "skipops",
		Ref: "refs/heads/main", SHA: testSHA, Trigger: periapsis.TriggerCI,
		Source:          loadPipeline(t, "release.yaml"),
		ApprovedSecrets: map[string]bool{},
	})
	if err == nil {
		t.Fatal("release.yaml with system-namespace paths was admitted for CI")
	}
	if !strings.Contains(err.Error(), "system-namespace path") {
		t.Fatalf("refusal for the wrong reason: %v", err)
	}
}

// TestOberthReleasePipelineWrapsCredentialedStepsWithSecretstoreExec holds the
// credentialed steps to the invocation the delivery chain actually requires:
//
//	oberth secretstore exec -> release.sh
//
// Each credentialed step invokes `oberth secretstore exec` with --path flags
// declaring exactly the vault paths that step needs. The exec subcommand
// handles authentication, file materialisation, env stripping, and redaction.
func TestOberthReleasePipelineWrapsCredentialedStepsWithSecretstoreExec(t *testing.T) {
	workflow, err := argoworkflow.Decode(loadPipeline(t, "release.yaml"))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	wrapped := 0
	for _, template := range workflow.Spec.Templates {
		if !templateUsesOberthSecretstore(&template) {
			continue
		}
		wrapped++
		paths := extractExecPaths(&template)
		if len(paths) == 0 {
			t.Errorf("template %q invokes secretstore exec with no --path flags", template.Name)
		}
		// Every path must be under the release namespace.
		for _, p := range paths {
			if !strings.HasPrefix(p, "oberth/data/release/") {
				t.Errorf("template %q declares path %q outside the release namespace", template.Name, p)
			}
		}
		// The command must end with -- release.sh <action>.
		allArgs := append(template.Container.Command, template.Container.Args...)
		foundSeparator := false
		for _, arg := range allArgs {
			if arg == "--" {
				foundSeparator = true
				break
			}
		}
		if !foundSeparator {
			t.Errorf("template %q has no -- separator before the release script", template.Name)
		}
		// Must have --dir flag.
		hasDir := false
		for _, arg := range allArgs {
			if strings.HasPrefix(arg, "--dir=") || arg == "--dir" {
				hasDir = true
				break
			}
		}
		if !hasDir {
			t.Errorf("template %q invokes secretstore exec without --dir", template.Name)
		}
		// Must not hardcode OBERTH_VAULT_ROLE (secretstore exec reads it from env).
		for _, arg := range allArgs {
			if strings.Contains(arg, "OBERTH_VAULT_ROLE") {
				t.Errorf("template %q references OBERTH_VAULT_ROLE in args; secretstore exec reads it from env", template.Name)
			}
		}
	}
	if wrapped == 0 {
		t.Fatal("no release template wraps its command with secretstore exec")
	}
}

// TestOberthReleasePublisherUsesPrivateChildDir pins the bootstrap path used by
// the deployed runner. Its older secretstore exec rejects a sticky emptyDir
// root, so each credentialed release leaf must create its own private child
// inside the server-owned memory volume before fetching any secret.
func TestOberthReleasePublisherUsesPrivateChildDir(t *testing.T) {
	built, err := Build(oberthConfig(), Request{
		RunID: "run-abc123", Name: "oberth-oberth-run-abc1-aabbccddeeff",
		Repo: "oberth", UpstreamOrg: "skipops",
		Ref: "refs/tags/v0.1.0", SHA: testSHA, Trigger: periapsis.TriggerRelease,
		Source:          loadPipeline(t, "release.yaml"),
		ApprovedSecrets: oberthReleaseApprovedSecrets(),
	})
	if err != nil {
		t.Fatalf("admit release.yaml: %v", err)
	}
	wantPaths := map[string][]string{
		"release-sign-binaries":    {"--path=oberth/data/release/cosign-secret"},
		"release-publish-homebrew": {"--path=oberth/data/release/homebrew-tap-key"},
		"release-publish-images":   {"--path=oberth/data/release/gar-image-key", "--path=oberth/data/release/cosign-secret"},
		"release-publish-chart":    {"--path=oberth/data/release/gar-chart-key", "--path=oberth/data/release/cosign-secret"},
		"release-verify":           {"--path=oberth/data/release/gar-reader-key"},
		"release-website":          {"--path=oberth/data/release/cloudflare-oberth-workers-token"},
	}
	seen := map[string]bool{}
	secretVolume := false
	for _, volume := range built.Spec.Volumes {
		if volume.Name != SecretsVolumeName {
			continue
		}
		secretVolume = volume.EmptyDir != nil && volume.EmptyDir.Medium == "Memory" &&
			volume.EmptyDir.SizeLimit != nil && volume.EmptyDir.SizeLimit.String() == secretsSizeLimit
	}
	if !secretVolume {
		t.Fatal("admitted release lacks its bounded server-owned memory volume")
	}
	for _, template := range built.Spec.Templates {
		if template.Container == nil {
			continue
		}
		if !templateUsesOberthSecretstore(&template) {
			if templateUsesCredentialChain(&template) {
				t.Errorf("credentialed template %q does not use the pinned secretstore invocation", template.Name)
			}
			for _, mount := range template.Container.VolumeMounts {
				if mount.Name == SecretsVolumeName || mount.Name == ReleaseTokenVolumeName {
					t.Errorf("non-credentialed template %q has credential mount %q", template.Name, mount.Name)
				}
			}
			continue
		}
		paths, expected := wantPaths[template.Name]
		if !expected {
			t.Errorf("unexpected credentialed template %q", template.Name)
			continue
		}
		seen[template.Name] = true
		if got := template.Container.Command; len(got) != 1 || got[0] != "/run/oberth/bin/oberth" {
			t.Errorf("template %q command = %v, want the server-seeded runner", template.Name, got)
		}
		wantArgs := append([]string{"secretstore", "exec", "--dir=" + SecretsMountPath + "/release"}, paths...)
		args := template.Container.Args
		if len(args) < len(wantArgs)+1 || strings.Join(args[:len(wantArgs)], "\x00") != strings.Join(wantArgs, "\x00") || args[len(wantArgs)] != "--" {
			prefix := args[:min(len(args), len(wantArgs)+1)]
			t.Errorf("template %q credential prefix = %v, want %v followed by --", template.Name, prefix, wantArgs)
		}
		if !strings.Contains(strings.Join(args, "\n"), `OBERTH_SECRETSTORE_DIR="$OBERTH_SECRETSTORE_DIR"`) {
			t.Errorf("template %q does not pass the selected private child directory to release.sh", template.Name)
		}
		secretMount := false
		for _, mount := range template.Container.VolumeMounts {
			if mount.Name == SecretsVolumeName && mount.MountPath == SecretsMountPath && !mount.ReadOnly {
				secretMount = true
			}
		}
		if !secretMount {
			t.Errorf("template %q lacks the writable server-owned memory volume", template.Name)
		}
	}
	for name := range wantPaths {
		if !seen[name] {
			t.Errorf("credentialed template %q not admitted", name)
		}
	}
}

// TestOberthReleasePipelineRetriesCredentialedSteps proves the publishing steps
// survive a transient failure.
//
// Every action behind these steps is idempotent by construction -- conditional
// PUTs, signature verification before signing, a compare-and-swap index loop --
// so a retry re-runs work that has already converged rather than duplicating
// it. Without a retry, a single 5xx from a registry or object store fails a
// release that has already published half its artifacts, and the recovery is a
// new tag.
func TestOberthReleasePipelineRetriesCredentialedSteps(t *testing.T) {
	workflow, err := argoworkflow.Decode(loadPipeline(t, "release.yaml"))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, template := range workflow.Spec.Templates {
		if template.Container == nil || !templateUsesCredentialChain(&template) {
			continue
		}
		if template.RetryStrategy == nil {
			t.Errorf("template %q publishes without a retryStrategy", template.Name)
			continue
		}
		if template.RetryStrategy.Limit == nil || template.RetryStrategy.Limit.IntValue() < 1 {
			t.Errorf("template %q declares a retryStrategy that never retries", template.Name)
		}
		// OnFailure retries a step whose container failed. OnError would also
		// retry infrastructure errors, which for a credentialed step includes a
		// rejected Vault login -- retrying that is a lockout, not a recovery.
		if template.RetryStrategy.RetryPolicy != wfv1.RetryPolicyOnFailure {
			t.Errorf("template %q retries with policy %q, want OnFailure",
				template.Name, template.RetryStrategy.RetryPolicy)
		}
		if template.RetryStrategy.Backoff == nil || template.RetryStrategy.Backoff.Duration == "" {
			t.Errorf("template %q retries with no backoff", template.Name)
		}
	}
}

// TestOberthPipelinesMountTheServerProvidedSource proves the documents rely on
// the server's read-only checkout rather than fetching source themselves.
func TestOberthPipelinesMountTheServerProvidedSource(t *testing.T) {
	for _, name := range []string{"build.yaml", "release.yaml"} {
		trigger := periapsis.TriggerCI
		approved := map[string]bool{}
		if name == "release.yaml" {
			trigger = periapsis.TriggerRelease
			approved = oberthReleaseApprovedSecrets()
		}
		built, err := Build(oberthConfig(), Request{
			RunID: "run-abc123", Name: "oberth-oberth-run-abc1-aabbccddeeff",
			Repo: "oberth", UpstreamOrg: "skipops", Ref: "refs/heads/main",
			SHA: testSHA, Trigger: trigger, Source: loadPipeline(t, name),
			// The claim the server seeded for this run, in the pipeline
			// namespace. Before this existed the Workflow named the server's
			// own claim, which no Pod in that namespace can mount.
			SourceVolume:    SourceVolume{ClaimName: "oberth-oberth-run-abc1-aabbccddeeff-src", SubPath: "src"},
			ApprovedSecrets: approved,
		})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		found := false
		for _, volume := range built.Spec.Volumes {
			if volume.Name != SourceVolumeName {
				continue
			}
			found = true
			if volume.PersistentVolumeClaim == nil || !volume.PersistentVolumeClaim.ReadOnly {
				t.Errorf("%s: the source volume is not a read-only claim", name)
			}
		}
		if !found {
			t.Errorf("%s: no server-provided source volume", name)
		}
		for _, template := range built.Spec.Templates {
			if template.Container == nil {
				continue
			}
			mounted := false
			for _, mount := range template.Container.VolumeMounts {
				if mount.Name == SourceVolumeName && mount.MountPath == SourceMountPath && mount.ReadOnly {
					mounted = true
				}
			}
			if !mounted {
				t.Errorf("%s: template %q does not mount the source read-only", name, template.Name)
			}
		}
	}
}

// assertBurnGraph checks that the entrypoint DAG's tasks and their depends
// expressions match the burn graph periapsis.go declares.
func assertBurnGraph(t *testing.T, workflow *wfv1.Workflow, entrypoint string, want map[string]string) {
	t.Helper()
	if workflow.Spec.Entrypoint != entrypoint {
		t.Fatalf("entrypoint = %q, want %q", workflow.Spec.Entrypoint, entrypoint)
	}
	var dag *wfv1.DAGTemplate
	for _, template := range workflow.Spec.Templates {
		if template.Name == entrypoint {
			dag = template.DAG
		}
	}
	if dag == nil {
		t.Fatalf("entrypoint template %q is not a DAG", entrypoint)
	}
	found := map[string]string{}
	for _, task := range dag.Tasks {
		found[task.Name] = task.Depends
	}
	for burn, depends := range want {
		got, present := found[burn]
		if !present {
			t.Errorf("burn %q is missing from the %s DAG", burn, entrypoint)
			continue
		}
		if got != depends {
			t.Errorf("burn %q depends on %q, want %q", burn, got, depends)
		}
	}
	for burn := range found {
		if _, expected := want[burn]; !expected {
			t.Errorf("burn %q is not declared in the expected %s DAG", burn, entrypoint)
		}
	}
}

// digestPinnedImage matches an image reference that names an exact Go patch
// release and an immutable digest, e.g.
// golang:1.26.6-trixie@sha256:<64 hex>.
var digestPinnedImage = regexp.MustCompile(`^golang:\d+\.\d+\.\d+-[a-z0-9.]+@sha256:[0-9a-f]{64}$`)

// pipelineImages returns every container image the document declares, keyed by
// the template that declares it.
func pipelineImages(t *testing.T, name string) map[string]string {
	t.Helper()
	workflow, err := argoworkflow.Decode(loadPipeline(t, name))
	if err != nil {
		t.Fatalf("decode %s: %v", name, err)
	}
	images := map[string]string{}
	for _, template := range workflow.Spec.Templates {
		if template.Container == nil {
			continue
		}
		images[template.Name] = template.Container.Image
	}
	if len(images) == 0 {
		t.Fatalf("%s declares no container templates", name)
	}
	return images
}

// TestOberthPipelinesPinCompatibleGoToolchains is the regression guard for the
// v0.12.5 release failure: release-publish-images' trivy scan found eight HIGH
// stdlib CVEs (CVE-2026-33818, CVE-2026-39821, CVE-2026-46600, CVE-2026-56853,
// CVE-2026-56858, CVE-2026-56859, CVE-2026-56860, CVE-2026-56862) in the
// shipped oberth binary, because release-build compiled it inside an image
// tagged golang:1.26-trixie whose digest was frozen at Go 1.26.5.
//
// Two properties keep that from recurring:
//
//   - The tag names the exact patch release, so the Go version that compiles
//     the released binary is readable in the pipeline text. A floating minor
//     tag pinned to a stale digest reads as current while shipping an old
//     stdlib -- that is what hid these CVEs.
//   - Branch CI uses the reviewed multi-platform index and release uses its
//     Linux amd64 child manifest. Both are Go 1.26.6 from the same image index.
//     The child runs release's amd64-only publisher tools on arm64 kind nodes;
//     CI stays native so its tests finish before the workflow deadline.
//     A partial image update fails here before an immutable tag is pushed.
func TestOberthPipelinesPinCompatibleGoToolchains(t *testing.T) {
	expected := map[string]string{
		"build.yaml":   "golang:1.26.6-trixie@sha256:ab563819a16cfe5faff0f96a8bb598fbb0e400ab2ac751996e60abcb23b106a3",
		"release.yaml": "golang:1.26.6-trixie@sha256:23fdfd3a6abc97c81e32a724cdd1cf541c06c416eb04d717815f4ed7c75623d0",
	}
	for document, want := range expected {
		count := 0
		for template, image := range pipelineImages(t, document) {
			if !strings.HasPrefix(image, "golang:") {
				// Every other image must still be digest-pinned.
				if !strings.Contains(image, "@sha256:") {
					t.Errorf("%s template %q image %q is not digest-pinned", document, template, image)
				}
				continue
			}
			if !digestPinnedImage.MatchString(image) {
				t.Errorf("%s template %q image %q must name an exact Go patch release and a digest, e.g. golang:1.26.6-trixie@sha256:<64 hex>",
					document, template, image)
				continue
			}
			count++
			if image != want {
				t.Errorf("%s template %q uses Go image %q, want %q", document, template, image, want)
			}
		}
		if count == 0 {
			t.Errorf("%s declares no Go toolchain image", document)
		}
	}
}

// TestOberthReleasePipelineScopesTokenToCredentialedTemplates verifies that
// Oberth's own release.yaml, built through the full admission chain, mounts
// the release-tier login token only in the templates that use the credential
// chain (secretstore exec or envconsul) and withholds it from non-credentialed
// templates (setup, lint, test, scan, chart-test, build, package-chart).
func TestOberthReleasePipelineScopesTokenToCredentialedTemplates(t *testing.T) {
	workflow, err := Build(oberthConfig(), Request{
		RunID: "run-abc123", Name: "oberth-oberth-run-abc1-aabbccddeeff",
		Repo: "oberth", UpstreamOrg: "skipops",
		Ref: "refs/tags/v0.1.0", SHA: testSHA, Trigger: periapsis.TriggerRelease,
		Source:          loadPipeline(t, "release.yaml"),
		ApprovedSecrets: oberthReleaseApprovedSecrets(),
	})
	if err != nil {
		t.Fatalf("admission: %v", err)
	}

	credentialed := 0
	noncredentialed := 0
	for _, template := range workflow.Spec.Templates {
		if template.Container == nil {
			continue
		}
		hasToken := false
		for _, mount := range template.Container.VolumeMounts {
			if mount.Name == ReleaseTokenVolumeName {
				hasToken = true
			}
		}
		if templateUsesCredentialChain(&template) {
			credentialed++
			if !hasToken {
				t.Errorf("credentialed template %q does not mount the release token", template.Name)
			}
		} else {
			noncredentialed++
			if hasToken {
				t.Errorf("non-credentialed template %q mounts the release token", template.Name)
			}
		}
	}
	if credentialed == 0 {
		t.Fatal("no credentialed templates found in release.yaml")
	}
	if noncredentialed == 0 {
		t.Fatal("no non-credentialed templates found; the test cannot verify scoping")
	}
}

// TestOberthReleaseScriptReadsStoreFieldNames pins every secret file
// release.sh opens to the store's actual KV field names.
//
// `oberth secretstore exec` writes each fetched field verbatim to
// $OBERTH_SECRETSTORE_DIR/<path-base>/<field> — there is no renaming layer
// (the retired oberth-secret-materialize shim was one, and its NAME=path/key
// mappings are why release.sh once read file-style names like
// r2-upload-token/token). The store's fields carry env-var-style names
// because the envconsul chain injected each field verbatim as an environment
// variable (docs/argo-secret-delivery.md). v0.13.1's release burn failed at
// its first credentialed step because release.sh still read the mapping-era
// names, which no gate before the step itself could catch.
//
// This test cannot see the live store; it pins the READER side to one
// reviewed declaration so any future rename is a conscious, two-sided change:
// editing this set is the documented signal that the store fields must be
// re-seeded to match in the same change window.
func TestOberthReleaseScriptReadsStoreFieldNames(t *testing.T) {
	script, err := os.ReadFile(filepath.Join(repositoryRoot(t), ".oberth", "release.sh"))
	if err != nil {
		t.Fatalf("read release.sh: %v", err)
	}
	// Every credential file read is $secret_root/<name>/<key>. The .runtime
	// scratch directory is derived credential state, not a store delivery.
	referencePattern := regexp.MustCompile(`\$secret_root/([A-Za-z0-9._-]+)/([A-Za-z0-9._-]+)`)
	expected := map[string]bool{
		"gar-image-key/GAR_SA_KEY":      true,
		"gar-chart-key/GAR_SA_KEY":      true,
		"gar-reader-key/GAR_SA_KEY":     true,
		"cosign-secret/COSIGN_KEY":      true,
		"cosign-secret/COSIGN_PASSWORD": true, // optional at runtime, named here so a rename is visible
		"cosign-secret/COSIGN_PUB":      true,
		"homebrew-tap-key/SSH_KEY":      true,
		// #649: the scoped oberth.ci Worker deploy token (Terraform
		// output oberth_website_deploy_token in oberthci/terraform).
		"cloudflare-oberth-workers-token/CLOUDFLARE_API_TOKEN": true,
	}
	seen := map[string]bool{}
	for _, line := range strings.Split(string(script), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		for _, match := range referencePattern.FindAllStringSubmatch(line, -1) {
			reference := match[1] + "/" + match[2]
			if match[1] == ".runtime" || match[2] == ".runtime" {
				continue
			}
			seen[reference] = true
			if !expected[reference] {
				t.Errorf("release.sh reads %q, which is not a declared store field: "+
					"either the read is wrong, or the store's KV fields changed and this "+
					"declaration (plus the store seeding) must move together", reference)
			}
		}
	}
	for reference := range expected {
		if !seen[reference] {
			t.Errorf("release.sh never reads %q; if the field was renamed, the store "+
				"seeding and this declaration must move together", reference)
		}
	}
}

// TestOberthReleasePathsCoverAllScriptReferences cross-references the --path
// declarations in release.yaml credentialed templates with the $secret_root
// references in release.sh. This catches a drift class where a path is
// declared in the workflow but never read by the script (wasted credential
// fetch) or read by the script but never declared (runtime failure at
// the secretstore exec step because the path was not fetched).
func TestOberthReleasePathsCoverAllScriptReferences(t *testing.T) {
	workflow, err := argoworkflow.Decode(loadPipeline(t, "release.yaml"))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	// Collect all path-bases from credentialed templates. extractExecPaths
	// only handles structured command/args arrays; some templates embed the
	// secretstore exec invocation inside a bash -c script body, so we also
	// scan the raw args text for --path= patterns.
	pathBases := map[string]bool{}
	pathPattern := regexp.MustCompile(`--path=([^\s\\]+)`)
	for index := range workflow.Spec.Templates {
		template := &workflow.Spec.Templates[index]
		for _, p := range extractExecPaths(template) {
			pathBases[filepath.Base(p)] = true
		}
		// Scan args text for embedded --path= in bash script bodies.
		if template.Container != nil {
			for _, arg := range template.Container.Args {
				for _, match := range pathPattern.FindAllStringSubmatch(arg, -1) {
					pathBases[filepath.Base(match[1])] = true
				}
			}
		}
		if template.Script != nil {
			for _, match := range pathPattern.FindAllStringSubmatch(template.Script.Source, -1) {
				pathBases[filepath.Base(match[1])] = true
			}
		}
	}
	if len(pathBases) == 0 {
		t.Fatal("no credentialed template declares --path flags")
	}

	// Collect all path-bases from release.sh $secret_root references.
	script, err := os.ReadFile(filepath.Join(repositoryRoot(t), ".oberth", "release.sh"))
	if err != nil {
		t.Fatalf("read release.sh: %v", err)
	}
	referencePattern := regexp.MustCompile(`\$secret_root/([A-Za-z0-9._-]+)/[A-Za-z0-9._-]+`)
	scriptBases := map[string]bool{}
	for _, line := range strings.Split(string(script), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		for _, match := range referencePattern.FindAllStringSubmatch(line, -1) {
			if match[1] != ".runtime" {
				scriptBases[match[1]] = true
			}
		}
	}
	if len(scriptBases) == 0 {
		t.Fatal("release.sh references no $secret_root paths")
	}

	// Every script reference must have a corresponding --path declaration.
	for base := range scriptBases {
		if !pathBases[base] {
			t.Errorf("release.sh reads from $secret_root/%s/ but no credentialed "+
				"template declares --path with base %q", base, base)
		}
	}
	// Every --path declaration should be referenced by the script.
	for base := range pathBases {
		if !scriptBases[base] {
			t.Errorf("a credentialed template declares --path with base %q but "+
				"release.sh never reads from $secret_root/%s/", base, base)
		}
	}
}
