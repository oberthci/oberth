package store

import (
	"errors"
	"testing"
	"time"
)

func TestSecretAccessRejectsUnsafeStepBeforeMutation(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	for _, step := range []string{"release\"", "release\npath", "release\\path", "release step", "release*"} {
		if _, err := s.Grant(t.Context(), "forge/org/oberth", ".oberth/release.yaml", step, "oberth/data/release", "operator"); !errors.Is(err, ErrInvalid) {
			t.Errorf("unsafe step %q: %v, want ErrInvalid", step, err)
		}
	}
	grants, err := s.SecretAccessList(t.Context(), "", true)
	if err != nil || len(grants) != 0 {
		t.Fatalf("invalid steps persisted: %+v, %v", grants, err)
	}
	for _, step := range []string{"*", "release", "build.test", "burn/step", "build_step"} {
		if _, err := s.Grant(t.Context(), "forge/org/oberth", ".oberth/release.yaml", step, "oberth/data/release", "operator"); err != nil {
			t.Fatalf("safe step %q: %v", step, err)
		}
	}
}
