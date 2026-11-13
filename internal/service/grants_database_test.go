package service

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
)

func TestGrantDatabaseAuthority(t *testing.T) {
	t.Run("ManualConfigMapEditsGrantNothing", testGrantConfigMapIsIgnored)
	t.Run("RevocationIsImmediate", testGrantRevocationIsImmediate)
}

func testGrantConfigMapIsIgnored(t *testing.T) {
	for _, operation := range []string{"Edit", "Recreate"} {
		t.Run(operation, func(t *testing.T) {
			f := newControlFixture(t)
			s := f.api(t)
			s.secretAccess = f.store
			actor := api.Actor{Identity: "admin", Admin: true}
			repo, err := f.store.QualifiedRepoName(t.Context(), f.repo.ID)
			if err != nil {
				t.Fatal(err)
			}
			const file = ".oberth/build.yaml"
			const approved = "oberth/data/release/approved"
			if _, err := s.accessAllow(t.Context(), actor, repo, file, "build", approved); err != nil {
				t.Fatal(err)
			}
			payload := func(path string) string {
				return fmt.Sprintf("- repo: %s\n  workflow: %s\n  step: build\n  secret: %s\n", repo, file, path)
			}
			cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "oberth-secret-access", Namespace: "oberth"}, Data: map[string]string{"grants": payload(approved)}}
			client := fake.NewClientset(cm)
			check := func() {
				t.Helper()
				rows, err := f.store.SecretAccessList(t.Context(), repo, false)
				if err != nil || len(rows) != 1 || rows[0].Secret != approved {
					t.Fatal("ConfigMap changed database authority", rows, err)
				}
				value, err := s.CallTool(t.Context(), actor, "admin_access_list", json.RawMessage(`{}`))
				if err != nil {
					t.Fatal(err)
				}
				raw, err := json.Marshal(value)
				if err != nil {
					t.Fatal(err)
				}
				var result struct {
					Grants []model.SecretAccessKey `json:"grants"`
				}
				if json.Unmarshal(raw, &result) != nil || len(result.Grants) != 1 || result.Grants[0].Workflow != file || result.Grants[0].Path != approved {
					t.Fatal("API did not read the database", string(raw))
				}
			}
			check()
			cm.Data["grants"] = payload("oberth/data/release/forged")
			if operation == "Edit" {
				if _, err := client.CoreV1().ConfigMaps("oberth").Update(t.Context(), cm, metav1.UpdateOptions{}); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := client.CoreV1().ConfigMaps("oberth").Delete(t.Context(), cm.Name, metav1.DeleteOptions{}); err != nil {
					t.Fatal(err)
				}
				if _, err := client.CoreV1().ConfigMaps("oberth").Create(t.Context(), cm, metav1.CreateOptions{}); err != nil {
					t.Fatal(err)
				}
			}
			check()
		})
	}
}

func testGrantRevocationIsImmediate(t *testing.T) {
	f := newControlFixture(t)
	s := f.api(t)
	s.secretAccess = f.store
	refreshes := 0
	s.secretAccessChanged = func(_ context.Context) error { refreshes++; return nil }
	actor := api.Actor{Identity: "admin", Admin: true}
	const repo = "codeberg/acme/oberth"
	const path = "oberth/upstream/acme/oberth/token"
	for _, file := range []string{".oberth/a.yaml", ".oberth/b.yaml"} {
		if _, err := s.accessAllow(t.Context(), actor, repo, file, "build", path); err != nil {
			t.Fatal(err)
		}
	}
	before, err := f.store.ActiveSecretGrants(t.Context(), f.repo.ID, ".oberth/a.yaml")
	if err != nil || !before["build"][path] {
		t.Fatal(before, err)
	}
	if _, err := s.accessRevoke(t.Context(), actor, repo, ".oberth/a.yaml", "build", path); err != nil {
		t.Fatal(err)
	}
	after, err := f.store.ActiveSecretGrants(t.Context(), f.repo.ID, ".oberth/a.yaml")
	if err != nil || after["build"][path] {
		t.Fatal("revoked grant survived the next admission read", after, err)
	}
	other, err := f.store.ActiveSecretGrants(t.Context(), f.repo.ID, ".oberth/b.yaml")
	if err != nil || !other["build"][path] {
		t.Fatal("another workflow changed", other, err)
	}
	if refreshes != 3 {
		t.Fatal("identity maps were not refreshed by committed writes", refreshes)
	}
}
