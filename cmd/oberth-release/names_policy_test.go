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

// A delivered identity reaches only the guards, as Git configuration after the step's own entries;
// without the file the guards keep the configuration they inherit.
func TestIdentityPolicyGivesOnlyTheGuardsTheRepositoryIdentity(t *testing.T) {
	workflow, err := argoworkflow.Decode(ownWorkflow(t, "build"))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, value, count string }{
		{"present", "Fixture Author <author@example.invalid>\n", "3"},
		{"absent", "", "1"},
		{"two-lines", "Fixture Author <author@example.invalid>\nOther <other@example.invalid>\n", ""},
		{"no-address", "Fixture Author\n", ""},
	} {
		t.Run(tc.name, func(t *testing.T) { checkStaticIdentity(t, workflow, tc.value, tc.count) })
	}
}

// checkStaticIdentity runs the step with an identity file holding value (none when empty); count "" expects a refusal.
func checkStaticIdentity(t *testing.T, workflow *wfv1.Workflow, value, count string) {
	t.Helper()
	dir := workflowFixture(t)
	values := staticFixture(t, dir)
	values["OBERTH_SECRETS_DIR"], values["EXPECTED_NAMES"] = staticPolicyFile(t, "present")
	values["EXPECTED_CONFIG_COUNT"], values["EXPECTED_IDENTITY"] = count, strings.TrimSpace(value)
	if value != "" {
		if err := os.WriteFile(filepath.Join(values["OBERTH_SECRETS_DIR"], "oberth/policy/identity"), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	output, err := executeWorkflowTemplate(t, workflow, workflow.GetTemplateByName("lint-and-guards-pass"), values)
	if count == "" {
		if err == nil || string(output) != "Identity must be one \"Name <email>\" line; store it again.\n" {
			t.Fatal("malformed identity did not refuse", err, string(output))
		}
		return
	}
	if err != nil || strings.Contains(string(output), "author@example.invalid") {
		t.Fatal("identity step failed or printed the identity", err, string(output))
	}
	assertStaticLog(t, values)
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
 *'run ./...') [ "$TMPDIR" = "$FIXTURE_ROOT/shared/tmp" ] || exit 93; echo lint >> "$STATIC_LOG";;
 *) exit 98;; esac
`)
	git := "'" + fixtureExecutable(t, "git") + "' config "
	writeFixtureCommand(t, filepath.Join(dir, "bin/python3"), `[ -z "${OBERTH_SECRETS_DIR:-}" ] || exit 97
case "$*" in
 'hack/guards/run.py') [ "${OBERTH_GUARDS_NAMES_FILE:-}" = "$EXPECTED_NAMES" ] && [ -s "$OBERTH_GUARDS_NAMES_FILE" ] || exit 97
  [ "$GIT_CONFIG_COUNT" = "${EXPECTED_CONFIG_COUNT:-1}" ] && `+git+`--get-all safe.directory | grep -qx /work/src || exit 95
  if [ -n "${EXPECTED_IDENTITY:-}" ]; then [ "$(`+git+`user.name) <$(`+git+`user.email)>" = "$EXPECTED_IDENTITY" ] || exit 94; fi
  echo guards >> "$STATIC_LOG";;
 'hack/plans/parity.py --check') [ -z "${OBERTH_GUARDS_NAMES_FILE:-}" ] && [ "$GIT_CONFIG_COUNT" = 1 ] || exit 97; echo parity >> "$STATIC_LOG";;
 *) exit 98;; esac
`)
	values := workflowValues(dir)
	values["STATIC_LOG"], values["OBERTH_GUARDS_NAMES_FILE"] = filepath.Join(dir, "static.log"), "inherited-must-not-leak"
	return values
}
