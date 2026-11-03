package service

import (
	"encoding/json"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/pkg/periapsis"
)

func TestResourceGrantToolAdminNumericAndLegacyIndependent(t *testing.T) {
	f := newControlFixture(t)
	s, err := NewAPI(APIConfig{Runs: f.store, Repositories: f.store, SizeGrants: f.store})
	if err != nil {
		t.Fatal(err)
	}
	raw := json.RawMessage(`{"repo":"oberth","requests":{"cpu":"1500m","memory":"2Gi"},"limits":{"cpu":"4","memory":"5Gi"}}`)
	admin := api.Actor{Identity: "admin", Admin: true}
	if _, err = s.CallTool(t.Context(), api.Actor{Identity: "reader"}, "resource_grant", raw); !errors.Is(err, ErrForbidden) {
		t.Fatalf("nonadmin grant: %v", err)
	}
	if _, err = s.CallTool(t.Context(), admin, "resource_grant", raw); err != nil {
		t.Fatal(err)
	}
	if _, err = f.store.SetSizeGrant(t.Context(), f.repo.ID, "admin", periapsis.XL); err != nil {
		t.Fatal(err)
	}
	value, err := s.CallTool(t.Context(), api.Actor{Identity: "reader"}, "resource_grant_get", json.RawMessage(`{"repo":"oberth"}`))
	if err != nil {
		t.Fatal(err)
	}
	ceiling := value.(map[string]any)["resource_ceiling"].(corev1.ResourceRequirements)
	if ceiling.Requests.Cpu().MilliValue() != 1500 || ceiling.Limits.Memory().Value() != 5<<30 {
		t.Fatal("legacy size mutation changed numeric ceiling")
	}
	if _, err = s.CallTool(t.Context(), admin, "resource_grant", json.RawMessage(`{"repo":"oberth","requests":{"cpu":"5","memory":"2Gi"},"limits":{"cpu":"4","memory":"5Gi"}}`)); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("invalid ceiling accepted: %v", err)
	}
	if _, err = s.CallTool(t.Context(), admin, "resource_revoke", json.RawMessage(`{"repo":"oberth"}`)); err != nil {
		t.Fatal(err)
	}
	ceiling, err = f.store.ResourceCeiling(t.Context(), f.repo.ID)
	if err != nil || ceiling.Requests.Cpu().MilliValue() != 4000 {
		t.Fatalf("revocation: %v", err)
	}
	if !MutatingTool("resource_grant") || !MutatingTool("resource_revoke") || MutatingTool("resource_grant_get") {
		t.Fatal("resource mutation gate classification")
	}
}
