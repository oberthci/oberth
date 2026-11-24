package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/service"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestLockLimitsAndCLIShowHeldPermits(t *testing.T) {
	o := serverOwners{cluster: clusterClients{Kube: fake.NewClientset(&core.ConfigMap{ObjectMeta: meta.ObjectMeta{Namespace: "runs", Name: "limits"}, Data: map[string]string{"heavy": "2"}})}}
	rows := []service.Lock{{Name: "runs/ConfigMap/limits/heavy"}, {Name: "runs/Mutex/repo"}}
	if err := o.lockLimits(t.Context(), rows); err != nil || rows[0].Limit != 2 || rows[1].Limit != 1 {
		t.Fatal(rows, err)
	}
	value := apicontract.SyncStatus{Locks: []apicontract.LockState{{Lock: rows[0].Name, Held: 2, Limit: 2, Usage: "2/2", Holders: []apicontract.LockHolder{{RunID: "build", Permits: 2, Steps: "compile, test"}}}}}
	raw, _ := json.Marshal(value)
	var out bytes.Buffer
	if err := (application{out: &out}).render(raw, false, commonOptions{}); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"2/2 held", "HOLDER", "PERMITS", "STEPS", "build", "2", "compile, test"} {
		if !strings.Contains(out.String(), text) {
			t.Fatal("CLI lost permit facts", out.String())
		}
	}
}

func TestLockLimitsPermitZeroCapacity(t *testing.T) {
	o := serverOwners{cluster: clusterClients{Kube: fake.NewClientset(&core.ConfigMap{ObjectMeta: meta.ObjectMeta{Namespace: "runs", Name: "limits"}, Data: map[string]string{"paused": "0"}})}}
	rows := []service.Lock{{Name: "runs/ConfigMap/limits/paused"}}
	if err := o.lockLimits(t.Context(), rows); err != nil || rows[0].Limit != 0 {
		t.Fatal("zero capacity lock cannot be inspected", rows, err)
	}
}
