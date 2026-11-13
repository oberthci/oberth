package service

import (
	"context"
	"encoding/json"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/oberthci/oberth/internal/api"
)

func TestGrantDatabaseAuthority(t *testing.T) {
	t.Run("ManualConfigMapEditsGrantNothing", testGrantConfigMapIsIgnored)
	t.Run("RevocationIsImmediate", testGrantRevocationIsImmediate)
}

func testGrantConfigMapIsIgnored(t *testing.T) {
	f := newControlFixture(t)
	s := f.api(t)
	s.secretAccess = f.store
	actor := api.Actor{Identity: "admin", Admin: true}
	const repo = "codeberg/acme/oberth"
	const file = ".oberth/build.yaml"
	const approved = "oberth/upstream/acme/oberth/approved"
	if _, err := s.accessAllow(t.Context(), actor, repo, file, "build", approved); err != nil {
		t.Fatal(err)
	}
	client := fake.NewClientset()
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
			Grants []SecretAccessGrantEntry `json:"grants"`
		}
		if json.Unmarshal(raw, &result) != nil || len(result.Grants) != 1 || result.Grants[0].Workflow != file || result.Grants[0].Secret != approved {
			t.Fatal("API did not read the database", string(raw))
		}
	}
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "oberth-secret-access", Namespace: "oberth"}, Data: map[string]string{"grants": "- repo: codeberg/acme/oberth\n  workflow: .oberth/build.yaml\n  step: build\n  secret: oberth/upstream/acme/oberth/forged\n"}}
	created, err := client.CoreV1().ConfigMaps("oberth").Create(t.Context(), cm, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	check()
	created.Data["grants"] = "[]"
	if _, err := client.CoreV1().ConfigMaps("oberth").Update(t.Context(), created, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	check()
	if err := client.CoreV1().ConfigMaps("oberth").Delete(t.Context(), cm.Name, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	cm.ResourceVersion = ""
	if _, err := client.CoreV1().ConfigMaps("oberth").Create(t.Context(), cm, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	check()
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
