package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/service"

	"github.com/oberthci/oberth/pkg/argoworkflow"
)

type failingAdmissionGrants struct{ calls int }

func (g *failingAdmissionGrants) ActiveSecretGrants(context.Context, int64, string) (map[string]map[string]bool, error) {
	g.calls++
	return nil, errors.New("secret grant store unavailable")
}

const admissionSecretPath = "oberth/upstream/acme/test-repo/token"

type admissionGrants struct {
	calls  int
	files  []string
	active map[string]map[string]bool
}

func (g *admissionGrants) ActiveSecretGrants(_ context.Context, _ int64, file string) (map[string]map[string]bool, error) {
	g.calls++
	g.files = append(g.files, file)
	return g.active, nil
}

func grantedAdmissionSource(t *testing.T, dir string) {
	t.Helper()
	file := filepath.Join(dir, ".oberth", "build.yaml")
	body, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	source := strings.Replace(string(body), "    example.com/tier: M", "    example.com/tier: M\n    oberth.ci/secret-paths: "+admissionSecretPath, 1)
	source = strings.Replace(source, `["echo", "ok"]`, `["oberth", "secretstore", "exec", "--path=`+admissionSecretPath+`", "--", "/bin/true"]`, 1)
	if err := os.WriteFile(file, []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestStepsWithoutSecretReadsIgnoreDegradedSecretAccess(t *testing.T) {
	for _, release := range []bool{false, true} {
		t.Run(map[bool]string{false: "branch", true: "release"}[release], func(t *testing.T) {
			f := newRaceFixture(t)
			g := &failingAdmissionGrants{}
			f.jobs.secretAccess = g
			f.jobs.SetReconcilerHealth(&fakeReconciler{healthy: false})
			r := f.request("plain", "plain-run")
			var err error
			if release {
				source, readErr := os.ReadFile(filepath.Join(f.sourceDir, ".oberth", "build.yaml"))
				if readErr != nil {
					t.Fatal(readErr)
				}
				writePipeline(t, f.sourceDir, "release.yaml", string(source))
				r.Run.Release = true
				err = f.jobs.CreateRelease(t.Context(), r)
			} else {
				err = f.jobs.CreateWorkflow(t.Context(), r)
			}
			if err != nil || g.calls != 0 || len(f.controller.created) != 1 {
				t.Fatalf("err=%v grant reads=%d created=%v", err, g.calls, f.controller.created)
			}
		})
	}
}

func TestGrantedStepsRequireHealthyReconciliation(t *testing.T) {
	f := newRaceFixture(t)
	grantedAdmissionSource(t, f.sourceDir)
	g := &admissionGrants{active: map[string]map[string]bool{"main": {admissionSecretPath: true}}}
	f.jobs.secretAccess = g
	f.jobs.SetReconcilerHealth(&fakeReconciler{healthy: false})
	err := f.jobs.CreateWorkflow(t.Context(), f.request("granted", "granted-run"))
	if len(g.files) != 1 || g.files[0] != argoworkflow.BuildFile {
		t.Fatal("admission loaded another workflow file", g.files)
	}
	if err == nil || !strings.Contains(err.Error(), "initial reconciliation; secret access is blocked") || g.calls != 1 || len(f.controller.created) != 0 {
		t.Fatalf("err=%v reads=%d created=%v", err, g.calls, f.controller.created)
	}
}

func TestSecretStepAdmissionReloadsCurrentGrants(t *testing.T) {
	f := newRaceFixture(t)
	grantedAdmissionSource(t, f.sourceDir)
	g := &admissionGrants{active: map[string]map[string]bool{"main": {admissionSecretPath: true}}}
	f.jobs.secretAccess = g
	body, err := os.ReadFile(filepath.Join(f.sourceDir, ".oberth", "build.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	wf, err := argoworkflow.Decode(body)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.jobs.stepSecretAccess(t.Context(), f.request("retry", "retry").Repository, wf, ".oberth/release.yaml"); err != nil {
		t.Fatal(err)
	}
	g.active = map[string]map[string]bool{}
	if _, _, err := f.jobs.stepSecretAccess(t.Context(), f.request("rerun", "rerun").Repository, wf, ".oberth/release.yaml"); err == nil {
		t.Fatal("revoked step grant was retained across attempts")
	}
	if g.calls != 2 {
		t.Fatalf("grant reads=%d", g.calls)
	}
}

func TestSecretConsumersRequireAvailableGrantLoader(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(map[bool]string{false: "unavailable", true: "missing"}[missing], func(t *testing.T) {
			f := newRaceFixture(t)
			grantedAdmissionSource(t, f.sourceDir)
			g := &failingAdmissionGrants{}
			f.jobs.secretAccess = g
			want := "load approved secrets for test-repo: secret grant store unavailable"
			if missing {
				f.jobs.secretAccess = nil
				want = "secret-consuming steps require a secret access grant loader"
			}
			err := f.jobs.CreateWorkflow(t.Context(), f.request("consumer", "consumer-run"))
			if err == nil || !strings.Contains(err.Error(), want) || len(f.controller.created) != 0 {
				t.Fatalf("err=%v created=%v", err, f.controller.created)
			}
		})
	}
}

type completingReconcileGrants struct{ health *fakeReconciler }

func (g completingReconcileGrants) ActiveSecretGrants(context.Context, int64, string) (map[string]map[string]bool, error) {
	// This read began while stale grants could still exist; reconciliation has
	// finished by the time it returns. The earlier unhealthy observation wins.
	g.health.healthy = true
	return map[string]map[string]bool{"main": {admissionSecretPath: true}}, nil
}

func TestGrantedStepsRejectGrantsReadBeforeInitialReconcile(t *testing.T) {
	f := newRaceFixture(t)
	grantedAdmissionSource(t, f.sourceDir)
	health := &fakeReconciler{healthy: false}
	f.jobs.SetReconcilerHealth(health)
	f.jobs.secretAccess = completingReconcileGrants{health: health}
	err := f.jobs.CreateWorkflow(t.Context(), f.request("reconcile-race", "reconcile-race-run"))
	if err == nil || !strings.Contains(err.Error(), "initial reconciliation") || len(f.controller.created) != 0 {
		t.Fatalf("stale grants became authoritative: err=%v created=%v", err, f.controller.created)
	}
}

func TestUnusedSecretDeclarationDoesNotBlock(t *testing.T) {
	for _, release := range []bool{false, true} {
		for _, declared := range []bool{false, true} {
			name := map[bool]string{false: "branch", true: "release"}[release] + "/" + map[bool]string{false: "plain", true: "unused-declaration"}[declared]
			t.Run(name, func(t *testing.T) {
				f := newRaceFixture(t)
				f.jobs.config.VaultAddress = "https://openbao.example"
				f.jobs.config.VaultCredentialedRole = "release-role"
				f.jobs.config.VaultCISecretsRole = "ci-role"
				g := &admissionGrants{active: map[string]map[string]bool{"main": {admissionSecretPath: true}}}
				f.jobs.secretAccess = g
				f.jobs.SetReconcilerHealth(&fakeReconciler{healthy: false})
				source, err := os.ReadFile(filepath.Join(f.sourceDir, ".oberth", "build.yaml"))
				if err != nil {
					t.Fatal(err)
				}
				if declared {
					source = []byte(strings.Replace(string(source), "    example.com/tier: M", "    example.com/tier: M\n    oberth.ci/secret-paths: "+admissionSecretPath, 1))
				}
				file := "build.yaml"
				if release {
					file = "release.yaml"
				}
				writePipeline(t, f.sourceDir, file, string(source))
				built := false
				f.jobs.beforeCreate = func(_ context.Context, _ service.WorkflowRequest, submission argojob.Request) error {
					wf, err := argojob.Build(f.jobs.config, submission)
					if err != nil {
						return err
					}
					built = true
					if wf.Spec.ServiceAccountName != f.jobs.config.PipelineServiceAccount {
						t.Error("unused annotation selected a store identity")
					}
					for _, volume := range wf.Spec.Volumes {
						if volume.Name == argojob.ReleaseTokenVolumeName || volume.Name == argojob.SecretsVolumeName {
							t.Errorf("ordinary workflow received %s", volume.Name)
						}
					}
					for _, template := range wf.Spec.Templates {
						if template.Container == nil {
							continue
						}
						for _, env := range template.Container.Env {
							if strings.HasPrefix(env.Name, "VAULT_") || env.Name == "OBERTH_VAULT_ROLE" {
								t.Errorf("ordinary step received %s", env.Name)
							}
						}
						for _, m := range template.Container.VolumeMounts {
							if m.Name == argojob.ReleaseTokenVolumeName || m.Name == argojob.SecretsVolumeName || m.MountPath == argojob.VaultCAMountPath || m.MountPath == argojob.OberthBinMountPath {
								t.Errorf("ordinary step received store mount %+v", m)
							}
						}
					}
					return nil
				}
				request := f.request("ordinary", "ordinary-run")
				request.Run.Release = release
				if release {
					err = f.jobs.CreateRelease(t.Context(), request)
				} else {
					err = f.jobs.CreateWorkflow(t.Context(), request)
				}
				if err != nil || g.calls != 0 || !built || len(f.controller.created) != 1 {
					t.Fatalf("err=%v reads=%d built=%v created=%v", err, g.calls, built, f.controller.created)
				}
			})
		}
	}
}
