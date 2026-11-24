package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/service"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestUnavailableLockLimitKeepsOtherLocksAndReadsMapOnce(t *testing.T) {
	kube := fake.NewClientset(&core.ConfigMap{ObjectMeta: meta.ObjectMeta{Namespace: "runs", Name: "limits"}, Data: map[string]string{"valid": "3", "invalid": "PRIVATE-NOT-A-NUMBER"}})
	o := serverOwners{cluster: clusterClients{Kube: kube}}
	rows := []service.Lock{{Name: "runs/ConfigMap/limits/valid"}, {Name: "runs/ConfigMap/limits/invalid"}, {Name: "runs/ConfigMap/limits/missing"}, {Name: "runs/ConfigMap/gone/heavy"}, {Name: "runs/Mutex/repo"}}
	o.lockLimits(t.Context(), rows)
	for i, want := range []int{3, -1, -1, -1, 1} {
		if rows[i].Limit != want {
			t.Fatal("unavailable capacity lost other locks", rows)
		}
	}
	if len(kube.Actions()) != 2 {
		t.Fatal("limits map read for every key", kube.Actions())
	}
	var out bytes.Buffer
	if err := renderLock(&out, map[string]any{"lock": rows[1].Name, "held": 2, "limit": nil}, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "2/? held") || strings.Contains(out.String(), "PRIVATE") {
		t.Fatal("unknown capacity hidden or leaked", out.String())
	}
}
