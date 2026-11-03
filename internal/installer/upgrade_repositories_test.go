package installer

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestUpgradeRequiresLiveRepositoryRetirementProof(t *testing.T) {
	for _, reply := range []string{`{"format":"oberth-retirement-check-v1","ready":true}`, `{"ready":true}`, `{"format":"oberth-retirement-check-v1","ready":false}`, `not JSON`, "unsupported"} {
		calls := 0
		err := checkUpgradeLegacyRepositories(t.Context(), UpgradeConfig{Namespace: "ci", BinaryVersion: "0.17.83"}, Deps{ContextName: "selected", RunCommand: func(ctx context.Context, stdin []byte, command string, args ...string) ([]byte, error) {
			calls++
			joined := strings.Join(args, " ")
			if command != "kubectl" || !strings.Contains(joined, "--context selected") || !strings.Contains(joined, "-n ci") || !strings.HasSuffix(joined, "admin retirement-check") || len(stdin) != 0 {
				t.Fatalf("not the live read-only check: %s %v", command, args)
			}
			if reply == "unsupported" {
				return []byte("repository legacy/source remains"), errors.New("refused")
			}
			return []byte(reply), nil
		}})
		if calls != 1 {
			t.Fatal("did not query live daemon")
		}
		if strings.Contains(reply, `"format":"oberth-retirement-check-v1","ready":true`) {
			if err != nil {
				t.Fatal(err)
			}
		} else if err == nil {
			t.Fatalf("unverified proof accepted: %s", reply)
		}
	}
}
