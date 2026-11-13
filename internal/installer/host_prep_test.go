package installer

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestHostPrepWarningsAreAdvisoryWithExactRemedies(t *testing.T) {
	for _, good := range []bool{false, true} {
		r := InstallResult{OK: true}
		swap := []byte("Filename Type Size Used Priority\n")
		instances := []byte("128")
		watches := []byte("8192")
		if good {
			swap = append(swap, []byte("/dev/zram0 partition 100000 0 100\n")...)
			instances = []byte("1024")
			watches = []byte("1048576")
		}
		addHostPrepWarnings("worker", swap, instances, watches, &r)
		if !r.OK {
			t.Fatal("host warning blocked install")
		}
		for _, c := range r.Checks {
			if c.Warning == good || c.OK != good || (!good && (c.Remedy == "" || c.Reason == "")) {
				t.Fatal(c)
			}
		}
	}
}

func TestHostPrepKubeletConfigUsesSelectedNodeAndWarnsOnUnknown(t *testing.T) {
	for _, good := range []bool{false, true} {
		r := InstallResult{OK: true}
		deps := testAdminDeps(Deps{ContextName: "selected", RunCommand: func(_ context.Context, _ []byte, name string, args ...string) ([]byte, error) {
			if name != "kubectl" || !strings.Contains(strings.Join(args, " "), "--context selected get --raw /api/v1/nodes/worker/proxy/configz") {
				t.Fatal(name, args)
			}
			if good {
				return []byte(`{"kubeletconfig":{"systemReserved":{"cpu":"500m","memory":"1Gi"},"evictionHard":{"memory.available":"500Mi"}}}`), nil
			}
			return []byte(`{}`), nil
		}})
		inspectHostPrep(t.Context(), Config{}, deps, corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker"}}, &r)
		if !r.OK || len(r.Checks) != 3 || r.Checks[2].OK != good {
			t.Fatal(r)
		}
	}
}
