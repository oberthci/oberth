package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/pipeline"
)

type failingAdmissionGrants struct{ calls int }

func (g *failingAdmissionGrants) ActiveSecretGrants(context.Context, int64) (map[string]map[string]bool, error) {
	g.calls++
	return nil, errors.New("secret grant store unavailable")
}

func TestUncredentialedBranchIgnoresDegradedSecretAccess(t *testing.T) {
	for _, healthy := range []bool{false, true} {
		t.Run(map[bool]string{false: "unhealthy", true: "healthy"}[healthy], func(t *testing.T) {
			fixture := newRaceFixture(t)
			grants := &failingAdmissionGrants{}
			fixture.jobs.secretAccess = grants
			fixture.jobs.SetReconcilerHealth(&fakeReconciler{healthy: healthy})
			if err := fixture.jobs.CreateWorkflow(t.Context(), fixture.request("plain", "plain-run")); err != nil {
				t.Fatalf("uncredentialed branch admission: %v", err)
			}
			if grants.calls != 0 || len(fixture.controller.created) != 1 {
				t.Fatalf("grant reads=%d, created=%v", grants.calls, fixture.controller.created)
			}
		})
	}
}

type admissionFragment struct{ source []byte }

func (f admissionFragment) Load(_ context.Context, key argoworkflow.FragmentKey, _ pipeline.Trigger, _ model.RefKind, _ string) (argoworkflow.Fragment, error) {
	return argoworkflow.Fragment{Key: key, SHA: strings.Repeat("b", 40), Source: f.source}, nil
}

func TestCredentialClassificationFailsClosedBeforeSecretAccess(t *testing.T) {
	for _, name := range []string{"branch-flag", "release", "promotion-flag", "declared-path", "secretstore", "inline-secretstore", "inline-wif", "fragment-secretstore", "promotion-secrets"} {
		t.Run(name, func(t *testing.T) {
			fixture := newRaceFixture(t)
			request := fixture.request("credentialed", "credentialed-run")
			file := filepath.Join(fixture.sourceDir, ".oberth", "build.yaml")
			body, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			source := string(body)
			switch name {
			case "branch-flag":
				request.Run.Credentialed = true
			case "release":
				request.Run.Release = true
				file = filepath.Join(fixture.sourceDir, ".oberth", "release.yaml")
			case "promotion-flag":
				request.Run.Trigger, request.Run.Credentialed = "promotion", true
			case "declared-path", "promotion-secrets":
				source = strings.Replace(source, "    example.com/tier: M", "    example.com/tier: M\n    oberth.ci/secret-paths: secret/example/ci", 1)
				if name == "promotion-secrets" {
					request.Run.Trigger = "promotion"
				}
			case "secretstore":
				source = strings.Replace(source, `["echo", "ok"]`, `["oberth", "secretstore", "exec"]`, 1)
			case "inline-secretstore", "inline-wif":
				leaf := "              container:\n                image: alpine:3.20\n                command: [oberth, secretstore, exec]\n"
				if name == "inline-wif" {
					leaf = "              metadata:\n                annotations:\n                  oberth.ci/release-wif-role: publisher\n              container:\n                image: alpine:3.20\n                command: [echo, ok]\n"
				}
				source = source[:strings.Index(source, "      container:")] + "      steps:\n        - - name: publish\n            inline:\n" + leaf
			case "fragment-secretstore":
				fixture.jobs.fragments = admissionFragment{source: []byte(strings.Replace(source, `["echo", "ok"]`, `["oberth", "secretstore", "exec"]`, 1))}
				source = source[:strings.Index(source, "      container:")] + "      steps:\n        - - name: publish\n            templateRef:\n              name: example/publish@v1\n              template: main\n"
			}
			if err := os.WriteFile(file, []byte(source), 0o644); err != nil {
				t.Fatal(err)
			}
			grants := &failingAdmissionGrants{}
			fixture.jobs.secretAccess = grants
			fixture.jobs.SetReconcilerHealth(&fakeReconciler{healthy: false})
			audited := false
			fixture.jobs.auditor = &stubAuditor{onAppend: func(model.AuditActionSpec) { audited = true }}
			if request.Run.Release {
				err = fixture.jobs.CreateRelease(t.Context(), request)
			} else {
				err = fixture.jobs.CreateWorkflow(t.Context(), request)
			}
			if err == nil || !strings.Contains(err.Error(), "initial reconciliation; credentialed admission is blocked for test-repo") {
				t.Fatalf("expected named reconciler health refusal, got %v", err)
			}
			if grants.calls != 0 || audited || len(fixture.controller.created) != 0 || len(fixture.jobs.intents) != 0 {
				t.Fatalf("refused admission had effects: grant reads=%d audit=%v created=%v intents=%v", grants.calls, audited, fixture.controller.created, fixture.jobs.intents)
			}
		})
	}
}

func TestCredentialedAdmissionStillRequiresSecretAccess(t *testing.T) {
	for _, missingLoader := range []bool{false, true} {
		t.Run(map[bool]string{false: "unavailable", true: "missing"}[missingLoader], func(t *testing.T) {
			fixture := newRaceFixture(t)
			request := fixture.request("credentialed", "credentialed-run")
			request.Run.Credentialed = true
			fixture.jobs.SetReconcilerHealth(&fakeReconciler{healthy: true})
			grants := &failingAdmissionGrants{}
			fixture.jobs.secretAccess = grants
			want := "load approved secrets for test-repo: secret grant store unavailable"
			if missingLoader {
				fixture.jobs.secretAccess = nil
				want = "ApprovedSecrets must be non-nil"
			}
			if err := fixture.jobs.CreateWorkflow(t.Context(), request); err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("expected fail-closed grant error %q, got %v", want, err)
			}
			if len(fixture.controller.created) != 0 || len(fixture.jobs.intents) != 0 || (!missingLoader && grants.calls != 1) {
				t.Fatalf("grant reads=%d created=%v intents=%v", grants.calls, fixture.controller.created, fixture.jobs.intents)
			}
		})
	}
}
