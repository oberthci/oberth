package service

import (
	"bytes"
	"encoding/json"
	"errors"
	"log"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/store"
)

func TestAccessToolsRequireAndPreserveWorkflowScope(t *testing.T) {
	f := newControlFixture(t)
	s := f.api(t)
	s.secretAccess = f.store
	actor := api.Actor{Identity: "admin", Admin: true}
	const repo = "upstream/org/repo"
	call := func(name, file string) (any, error) {
		args := map[string]string{"repo": repo, "step": "publish", "secret": "oberth/data/release/token"}
		if file != "" {
			args["workflow"] = file
		}
		raw, _ := json.Marshal(args)
		return s.CallTool(t.Context(), actor, name, raw)
	}
	for _, name := range []string{"access_allow", "access_revoke"} {
		if _, err := call(name, ""); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("%s accepted absent workflow: %v", name, err)
		}
	}
	for _, file := range []string{".oberth/a.yaml", ".oberth/b.yaml"} {
		value, err := call("access_allow", file)
		if err != nil || value.(api.AccessGrantResponse).Workflow != file {
			t.Fatal(value, err)
		}
	}
	if _, err := call("access_revoke", ".oberth/a.yaml"); err != nil {
		t.Fatal(err)
	}
	listed, err := s.CallTool(t.Context(), actor, "access_list", json.RawMessage(`{"repo":"upstream/org/repo"}`))
	if err != nil {
		t.Fatal(err)
	}
	grants := listed.(api.AccessListResponse).Grants
	if len(grants) != 1 || grants[0].Workflow != ".oberth/b.yaml" {
		t.Fatal("revoke crossed files", grants)
	}
}

func TestReconcileRejectsUnscopedConfigMapApprovals(t *testing.T) {
	const original = `
- repo: upstream/org/repo
  step: publish
  secret: oberth/data/release/old-token
- repo: upstream/org/repo
  workflow: .oberth/release.yaml
  step: publish
  secret: oberth/data/release/new-token
`
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: secretAccessConfigMapName, Namespace: "oberth"}, Data: map[string]string{secretAccessConfigMapKey: original}}
	r, database := testAccessReconciler(t, cm)
	for range 2 {
		if err := r.Reconcile(t.Context()); !errors.Is(err, ErrUnscopedSecretGrant) || !strings.Contains(err.Error(), "workflow is required") {
			t.Fatalf("unscoped approval was not rejected: %v", err)
		}
		if r.ReconcileHealthy() {
			t.Fatal("unscoped approval made reconciliation healthy")
		}
		grants, err := database.SecretAccessList(t.Context(), "", false)
		if err != nil || len(grants) != 0 {
			t.Fatal("rejected approvals became authority", grants, err)
		}
		saved, err := r.client.CoreV1().ConfigMaps("oberth").Get(t.Context(), secretAccessConfigMapName, metav1.GetOptions{})
		if err != nil || saved.Data[secretAccessConfigMapKey] != original {
			t.Fatal("reconcile silently edited unscoped approvals", saved, err)
		}
	}
}

// A startup receipt is supplied by the real Store in production. This wrapper
// isolates the one-time reconciliation boundary from migration SQL tests.
type scopeMigrationAccessStore struct {
	SecretAccessStore
	applied bool
}

func (s scopeMigrationAccessStore) AppliedStartupMigration(id string) bool {
	return s.applied && id == store.WorkflowGrantScopeMigrationID
}

func TestScopeMigrationClearsUnscopedConfigMapOnce(t *testing.T) {
	const original = `
- repo: upstream/org/repo
  step: publish
  secret: oberth/data/release/old-token
- repo: upstream/org/repo
  workflow: .oberth/release.yaml
  step: publish
  secret: oberth/data/release/new-token
`
	for _, applied := range []bool{false, true} {
		t.Run(map[bool]string{false: "no-startup-receipt", true: "committed-upgrade"}[applied], func(t *testing.T) {
			cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: secretAccessConfigMapName, Namespace: "oberth"}, Data: map[string]string{secretAccessConfigMapKey: original, "keep": "public"}}
			base, db := testAccessReconciler(t, cm)
			var logs bytes.Buffer
			r := NewAccessReconciler(base.client, "oberth", scopeMigrationAccessStore{SecretAccessStore: db, applied: applied}, log.New(&logs, "", 0))
			err := r.Reconcile(t.Context())
			if !applied {
				if !errors.Is(err, ErrUnscopedSecretGrant) {
					t.Errorf("cleanup without THIS-start receipt: %v", err)
				}
				saved, getErr := r.client.CoreV1().ConfigMaps("oberth").Get(t.Context(), secretAccessConfigMapName, metav1.GetOptions{})
				if getErr != nil || saved.Data[secretAccessConfigMapKey] != original {
					t.Fatal("no-receipt reconciliation edited approvals", saved, getErr)
				}
				return
			}
			if err != nil {
				t.Fatal("upgrade cleanup did not allow re-approval", err)
			}
			if !r.ReconcileHealthy() {
				t.Fatal("upgrade cleanup left reconciliation unhealthy")
			}
			if !strings.Contains(logs.String(), "removed 1 unscoped entries from ConfigMap oberth/oberth-secret-access") {
				t.Fatal("cleanup count missing", logs.String())
			}
			saved, err := r.client.CoreV1().ConfigMaps("oberth").Get(t.Context(), secretAccessConfigMapName, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if saved.Data["keep"] != "public" || strings.Contains(saved.Data[secretAccessConfigMapKey], "old-token") {
				t.Fatal("cleanup lost unrelated data or retained old approval", saved.Data)
			}
			grants, err := db.SecretAccessList(t.Context(), "", false)
			if err != nil || len(grants) != 1 || grants[0].Secret != "oberth/data/release/new-token" {
				t.Fatal("upgrade restored a pre-scope approval", grants, err)
			}
			// The operator's advertised re-approval command now has a writable map.
			err = r.UpdateConfigMap(t.Context(), "admin", func(entries []SecretAccessGrantEntry) ([]SecretAccessGrantEntry, error) {
				return append(entries, SecretAccessGrantEntry{Repo: "upstream/org/repo", Workflow: ".oberth/release.yaml", Step: "publish", Secret: "oberth/data/release/reapproved"}), nil
			})
			if err != nil {
				t.Fatal("valid re-approval blocked after cleanup", err)
			}
			saved, err = r.client.CoreV1().ConfigMaps("oberth").Get(t.Context(), secretAccessConfigMapName, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			saved.Data[secretAccessConfigMapKey] += "\n- repo: upstream/org/repo\n  step: publish\n  secret: oberth/data/release/late-token\n"
			if _, err = r.client.CoreV1().ConfigMaps("oberth").Update(t.Context(), saved, metav1.UpdateOptions{}); err != nil {
				t.Fatal(err)
			}
			err = r.Reconcile(t.Context())
			if !errors.Is(err, ErrUnscopedSecretGrant) {
				t.Fatalf("later unscoped entry was silently cleared: %v", err)
			}
			for _, part := range []string{"oberth/oberth-secret-access", "entry 2", "kubectl -n oberth edit configmap oberth-secret-access"} {
				if !strings.Contains(err.Error(), part) {
					t.Errorf("rejection lacks %q: %v", part, err)
				}
			}
			err = r.UpdateConfigMap(t.Context(), "admin", func(entries []SecretAccessGrantEntry) ([]SecretAccessGrantEntry, error) { return entries, nil })
			if !errors.Is(err, ErrUnscopedSecretGrant) || !strings.Contains(err.Error(), "oberth/oberth-secret-access") {
				t.Fatalf("tool rejection lacks ConfigMap remedy: %v", err)
			}
			if strings.Count(logs.String(), "removed 1 unscoped entries") != 1 {
				t.Fatal("cleanup ran more than once", logs.String())
			}
		})
	}
	t.Run("absent-map-consumes-startup-permission", func(t *testing.T) {
		base, db := testAccessReconciler(t, nil)
		r := NewAccessReconciler(base.client, "oberth", scopeMigrationAccessStore{SecretAccessStore: db, applied: true}, log.New(&bytes.Buffer{}, "", 0))
		if err := r.Reconcile(t.Context()); err != nil {
			t.Fatal(err)
		}
		cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: secretAccessConfigMapName, Namespace: "oberth"}, Data: map[string]string{secretAccessConfigMapKey: original}}
		if _, err := r.client.CoreV1().ConfigMaps("oberth").Create(t.Context(), cm, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
		if err := r.Reconcile(t.Context()); !errors.Is(err, ErrUnscopedSecretGrant) {
			t.Fatalf("late map reused startup cleanup permission: %v", err)
		}
	})
}
