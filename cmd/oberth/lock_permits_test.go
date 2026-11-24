package main

import (
	"bytes"
	"encoding/json"
	"slices"
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
	value := apicontract.SyncStatus{Locks: []apicontract.LockState{{Lock: rows[0].Name, Held: 9, Limit: 12, Usage: "9/12", Holders: []apicontract.LockHolder{{RunID: "build", Permits: 7, Steps: "compile, test"}, {RunID: "lint", Permits: 2, Steps: "lint"}}}}}
	raw, _ := json.Marshal(value)
	var out bytes.Buffer
	if err := (application{out: &out}).render(raw, false, commonOptions{}); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"9/12 held", "HOLDER", "PERMITS", "STEPS", "build", "compile, test"} {
		if !strings.Contains(out.String(), text) {
			t.Fatal("CLI lost permit facts", out.String())
		}
	}
	for _, line := range strings.Split(out.String(), "\n") {
		fields := strings.Fields(line)
		if len(fields) > 0 && fields[0] == "build" {
			if !slices.Equal(fields, []string{"build", "7", "compile,", "test"}) {
				t.Fatal("PERMITS cell lost", line)
			}
			return
		}
	}
	t.Fatal("holder row missing", out.String())
}

func TestLockLimitsPermitZeroCapacity(t *testing.T) {
	o := serverOwners{cluster: clusterClients{Kube: fake.NewClientset(&core.ConfigMap{ObjectMeta: meta.ObjectMeta{Namespace: "runs", Name: "limits"}, Data: map[string]string{"paused": "0"}})}}
	rows := []service.Lock{{Name: "runs/ConfigMap/limits/paused"}}
	if err := o.lockLimits(t.Context(), rows); err != nil || rows[0].Limit != 0 {
		t.Fatal("zero capacity lock cannot be inspected", rows, err)
	}
}
