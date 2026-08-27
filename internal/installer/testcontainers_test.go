package installer

import (
	"strings"
	"testing"
)

// The shim and the egress rule only work together. One --set without the other
// produces a deployment where Testcontainers hangs on its wait strategy, which
// is exactly what the two-manual-steps arrangement kept producing.
func TestTestcontainersDeploysTheShimAndOpensTheEgressTogether(t *testing.T) {
	t.Parallel()
	args := strings.Join(OberthHelmArgs(Config{Testcontainers: true}, OpenBaoResult{}, RekorResult{}), " ")
	for _, want := range []string{"kubedock.enabled=true", "networkPolicy.inNamespaceAllPorts=true"} {
		if !strings.Contains(args, want) {
			t.Fatalf("helm args missing %q:\n%s", want, args)
		}
	}

	without := strings.Join(OberthHelmArgs(Config{}, OpenBaoResult{}, RekorResult{}), " ")
	if strings.Contains(without, "kubedock") || strings.Contains(without, "inNamespaceAllPorts") {
		t.Fatalf("an ordinary install carries the preset anyway:\n%s", without)
	}
}
