package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/pkg/argoworkflow"
)

// policyPaths are the guard step's two delivered files: the private names and the repository identity.
func policyPaths() []string {
	return []string{"oberth/policy/names", "oberth/policy/identity"}
}

func policyGrants(r admissioncontract.PrepareRequest) []admissioncontract.GrantEvidence {
	grants := []admissioncontract.GrantEvidence{}
	for _, path := range policyPaths() {
		grants = append(grants, admissioncontract.GrantEvidence{Repository: r.Repository, File: r.SelectedFile, Step: "lint-and-guards-pass", Kind: "secret", Path: path, Revision: "1", State: admissioncontract.StateVerified})
	}
	return grants
}

func TestNamesPolicyUsesGrantedStepAndBuiltCLI(t *testing.T) {
	workflow, err := argoworkflow.Decode(ownWorkflow(t, "build"))
	if err != nil {
		t.Fatal(err)
	}
	step := workflow.GetTemplateByName("lint-and-guards-pass")
	if step == nil || step.Container.Command[0] != "/work/shared/oberth" {
		t.Fatal("static task does not use its built CLI")
	}
	t.Run("native-login", func(t *testing.T) { assertNoAuthoredStoreLogin(t, step) })
	t.Run("arguments", func(t *testing.T) { assertSigningCLI(t, step.Container) })
	prepared, err := prepareOwn(t, "build", model.TriggerBranch)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range prepared.View().Plan.Entries {
		if entry.CredentialIntent != (entry.Name == "lint-and-guards-pass") {
			t.Fatal("names policy credential intent reached another step", entry.Name)
		}
	}
}

func TestPolicyMissingGrantRefusesAdmission(t *testing.T) {
	for i, path := range policyPaths() {
		t.Run(path, func(t *testing.T) {
			r := ownRequest(ownWorkflow(t, "build"), "build", model.TriggerBranch)
			r.Grants = append(policyGrants(r)[:i:i], policyGrants(r)[i+1:]...)
			prepared, err := argoworkflow.Prepare(t.Context(), r, ownPolicy(), ownHooks(t, r))
			want := "oberth secret put --repo 'forge/oberthci/oberth' --file '.oberth/build.yaml' --step 'lint-and-guards-pass' '" + path + "' < <value-file>"
			if prepared != nil || err == nil || !strings.Contains(err.Error(), want) {
				t.Fatal("missing policy grant did not refuse admission with the exact grant", prepared, err)
			}
		})
	}
}

func TestNamesPolicyWrittenStepScopesFileToGuard(t *testing.T) {
	workflow, err := argoworkflow.Decode(ownWorkflow(t, "build"))
	if err != nil {
		t.Fatal(err)
	}
	for _, which := range []string{"present", "no-delivery", "unset-delivery", "missing-file", "empty-file"} {
		t.Run(which, func(t *testing.T) { checkStaticNames(t, workflow, which) })
	}
}

func checkStaticNames(t *testing.T, workflow *wfv1.Workflow, which string) {
	t.Helper()
	dir := workflowFixture(t)
	values := staticFixture(t, dir)
	values["OBERTH_SECRETS_DIR"], values["EXPECTED_NAMES"] = staticPolicyFile(t, which)
	if which == "unset-delivery" {
		t.Setenv("OBERTH_SECRETS_DIR", "inherited-delivery")
		delete(values, "OBERTH_SECRETS_DIR")
		writeFixtureCommand(t, filepath.Join(dir, "shared/oberth"), `shift 3
unset OBERTH_SECRETS_DIR
exec "$@"
`)
	}
	output, err := executeWorkflowTemplate(t, workflow, workflow.GetTemplateByName("lint-and-guards-pass"), values)
	if which != "present" {
		if err == nil || string(output) != "Names policy is missing; store and grant it for this step.\n" {
			t.Fatal("missing policy did not refuse", err, string(output))
		}
		if _, err := os.Stat(values["STATIC_LOG"]); !os.IsNotExist(err) {
			t.Fatal("static commands ran without the file", err)
		}
		return
	}
	if err != nil {
		t.Fatal(err, string(output))
	}
	assertStaticLog(t, values)
}

const (
	fixtureIdentity  = "Fixture Author <author@example.invalid>"
	identityMissing  = "Identity oberth/policy/identity is missing; store and grant it.\n"
	identityMalformed = "Identity must be one \"Name <email>\" line; store it again.\n"
)

// The delivered identity is required and reaches only the guards, as Git configuration after the
// step's own entries; a missing or malformed file refuses before any command runs.
func TestIdentityPolicyGivesOnlyTheGuardsTheRepositoryIdentity(t *testing.T) {
	workflow, err := argoworkflow.Decode(ownWorkflow(t, "build"))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, refusal string }{
		{"present", ""}, {"absent", identityMissing}, {"empty", identityMissing}, {"directory", identityMissing},
		{"two-lines", identityMalformed}, {"no-address", identityMalformed},
	} {
		t.Run(tc.name, func(t *testing.T) { checkStaticIdentity(t, workflow, tc.name, tc.refusal) })
	}
}

// checkStaticIdentity replaces the fixture's identity file as which says and runs the step.
func checkStaticIdentity(t *testing.T, workflow *wfv1.Workflow, which, refusal string) {
	t.Helper()
	dir := workflowFixture(t)
	values := staticFixture(t, dir)
	values["OBERTH_SECRETS_DIR"], values["EXPECTED_NAMES"] = staticPolicyFile(t, "present")
	replaceIdentity(t, filepath.Join(values["OBERTH_SECRETS_DIR"], "oberth/policy/identity"), which)
	output, err := executeWorkflowTemplate(t, workflow, workflow.GetTemplateByName("lint-and-guards-pass"), values)
	if refusal != "" {
		if _, statErr := os.Stat(values["STATIC_LOG"]); err == nil || string(output) != refusal || !os.IsNotExist(statErr) {
			t.Fatal("identity did not refuse before any command", err, string(output), statErr)
		}
		return
	}
	if err != nil || strings.Contains(string(output), "author@example.invalid") {
		t.Fatal("identity step failed or printed the identity", err, string(output))
	}
	assertStaticLog(t, values)
}

// replaceIdentity turns the fixture's valid identity file into the case which names.
func replaceIdentity(t *testing.T, file, which string) {
	t.Helper()
	contents := map[string]string{"empty": "", "two-lines": fixtureIdentity + "\nOther <other@example.invalid>\n", "no-address": "Fixture Author\n"}
	if value, ok := contents[which]; ok {
		writeIdentity(t, file, value)
	}
	if which == "absent" || which == "directory" {
		if err := os.Remove(file); err != nil {
			t.Fatal(err)
		}
	}
	if which == "directory" {
		if err := os.Mkdir(file, 0700); err != nil {
			t.Fatal(err)
		}
	}
}

func writeIdentity(t *testing.T, file, value string) {
	t.Helper()
	if err := os.WriteFile(file, []byte(value), 0600); err != nil {
		t.Fatal(err)
	}
}

func assertStaticLog(t *testing.T, values map[string]string) {
	t.Helper()
	body, err := os.ReadFile(values["STATIC_LOG"])
	if err != nil || string(body) != "diff\nlint\nguards\nparity\n" {
		t.Fatal("static commands differ", err, string(body))
	}
}

func staticPolicyFile(t *testing.T, which string) (string, string) {
	t.Helper()
	delivery := t.TempDir()
	file := filepath.Join(delivery, "oberth/policy/names")
	if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
		t.Fatal(err)
	}
	if which == "present" || which == "empty-file" {
		value := "fixture-private-name\n"
		if which == "empty-file" {
			value = ""
		}
		if err := os.WriteFile(file, []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	writeIdentity(t, filepath.Join(delivery, "oberth/policy/identity"), fixtureIdentity+"\n")
	if which == "no-delivery" {
		delivery = ""
	}
	return delivery, file
}

func staticFixture(t *testing.T, dir string) map[string]string {
	t.Helper()
	writeFixtureCommand(t, filepath.Join(dir, "shared/oberth"), `[ "$1 $2 $3" = 'secretstore exec --' ] || exit 96
shift 3
exec "$@"
`)
	writeFixtureCommand(t, filepath.Join(dir, "bin/git"), `[ -z "${OBERTH_GUARDS_NAMES_FILE:-}${OBERTH_SECRETS_DIR:-}" ] && [ "$GIT_CONFIG_COUNT" = 1 ] || exit 97
echo diff >> "$STATIC_LOG"
`)
	writeFixtureCommand(t, filepath.Join(dir, "bin/go"), `[ -z "${OBERTH_GUARDS_NAMES_FILE:-}${OBERTH_SECRETS_DIR:-}" ] && [ "$GIT_CONFIG_COUNT" = 1 ] || exit 97
case "$*" in *'version --short') cat hack/lint-version | sed 's/^v//';;
 *'run '*./...) [ "$TMPDIR" = "$FIXTURE_ROOT/shared/tmp" ] && [ "$GOLANGCI_LINT_CACHE" = "$FIXTURE_ROOT/cache/golangci-lint" ] || exit 93
  echo lint >> "$STATIC_LOG";;
 *) exit 98;; esac
`)
	git := "'" + fixtureExecutable(t, "git") + "' config "
	writeFixtureCommand(t, filepath.Join(dir, "bin/python3"), `[ -z "${OBERTH_SECRETS_DIR:-}" ] || exit 97
if [ "${1:-}" = -I ]; then shift; fi
case "$*" in
 *'hack/tools/go_tool.py lint version --short') [ -z "${OBERTH_GUARDS_NAMES_FILE:-}" ] && [ "$GIT_CONFIG_COUNT" = 1 ] || exit 97
  cat hack/lint-version | sed 's/^v//';;
 *'hack/tools/go_tool.py lint run --config .golangci.yml ./...') [ -z "${OBERTH_GUARDS_NAMES_FILE:-}" ] && [ "$GIT_CONFIG_COUNT" = 1 ] || exit 97
  [ "$TMPDIR" = "$FIXTURE_ROOT/shared/tmp" ] && [ "$GOLANGCI_LINT_CACHE" = "$FIXTURE_ROOT/cache/golangci-lint" ] || exit 93; echo lint >> "$STATIC_LOG";;
 'hack/guards/run.py') [ "${OBERTH_GUARDS_NAMES_FILE:-}" = "$EXPECTED_NAMES" ] && [ -s "$OBERTH_GUARDS_NAMES_FILE" ] || exit 97
  [ "$GIT_CONFIG_COUNT" = 3 ] && `+git+`--get-all safe.directory | grep -qx /work/src || exit 95
  [ "$(`+git+`user.name) <$(`+git+`user.email)>" = '`+fixtureIdentity+`' ] || exit 94
  echo guards >> "$STATIC_LOG";;
 'hack/plans/parity.py --check') [ -z "${OBERTH_GUARDS_NAMES_FILE:-}" ] && [ "$GIT_CONFIG_COUNT" = 1 ] || exit 97; echo parity >> "$STATIC_LOG";;
 *) exit 98;; esac
`)
	values := workflowValues(dir)
	values["STATIC_LOG"], values["OBERTH_GUARDS_NAMES_FILE"] = filepath.Join(dir, "static.log"), "inherited-must-not-leak"
	// A step has no home directory: every tool default under HOME must be replaced by the workflow.
	values["HOME"] = "/nonexistent"
	return values
}
