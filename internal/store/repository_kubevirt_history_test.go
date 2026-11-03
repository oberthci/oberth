package store

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestRepositoryRemovalExplainsLegacyKubeVirtGrant(t *testing.T) {
	for _, revoked := range []bool{false, true} {
		t.Run(map[bool]string{false: "active", true: "revoked"}[revoked], func(t *testing.T) {
			now := time.Now()
			s := testStore(t, &now)
			repo := createRepo(t, s)
			if _, err := s.SetKubeVirtGrant(t.Context(), repo.ID, "admin", true, KubeVirtBudget{}); err != nil {
				t.Fatal(err)
			}
			if revoked {
				if _, err := s.SetKubeVirtGrant(t.Context(), repo.ID, "admin", false, KubeVirtBudget{}); err != nil {
					t.Fatal(err)
				}
			}
			_, err := s.RemoveRepository(t.Context(), "admin", repo.Name)
			if !errors.Is(err, ErrInvalidState) || !strings.Contains(err.Error(), "KubeVirt") || !strings.Contains(err.Error(), "archive") {
				t.Fatalf("legacy grant removal did not explain retained history: %v", err)
			}
			if _, err = s.Repository(t.Context(), repo.ID); err != nil {
				t.Fatalf("refusal deleted repository: %v", err)
			}
			grant, err := s.KubeVirtGrant(t.Context(), repo.ID)
			if err != nil || (grant.RevokedAt != nil) != revoked {
				t.Fatalf("refusal changed grant: %+v %v", grant, err)
			}
		})
	}
}
