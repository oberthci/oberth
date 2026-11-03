package service

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/pkg/argoworkflow"
)

type projectedVMIEngine struct {
	*groupEngine
	source    []byte
	fragments map[argoworkflow.FragmentKey]argoworkflow.Fragment
	receipts  sync.Map
}

func (e *projectedVMIEngine) PipelineKubeVirt(_ context.Context, r JobRequest) (KubeVirtResources, error) {
	resolved, err := argoworkflow.ResolveRunSource(e.source, e.fragments, argoworkflow.RunSourceBinding{RunID: r.Run.ID, RepositoryID: r.Repository.ID, SHA: r.Run.SHA, File: argoworkflow.BuildFile, Trigger: argoworkflow.RunTriggerBranch})
	if err != nil {
		return KubeVirtResources{}, err
	}
	receipt, err := resolved.Receipt(argoworkflow.Policy{KubeVirt: true, ContainerDiskPrefixes: []string{"quay.io/example/"}, RunnerImagePrefixes: []string{"busybox:"}})
	if err != nil {
		return KubeVirtResources{}, err
	}
	e.receipts.Store(r.Run.ID, receipt)
	return KubeVirtResources{Demand: receipt.Summary().Demand, Limit: argoworkflow.VMIDemand{VMs: 4, Cores: 8, MemoryBytes: 24 << 30}, Receipt: receipt}, nil
}
func (e *projectedVMIEngine) CreateCI(_ context.Context, r JobRequest) error {
	receipt, ok := e.receipts.Load(r.Run.ID)
	if !ok || receipt != r.VMIDemandReceipt {
		return fmt.Errorf("lost run-aware demand receipt for %s", r.Run.ID)
	}
	return e.create(r)
}

func TestKubeVirtSchedulerRunAwareProjectionAndReceipt(t *testing.T) {
	f := newGroupFixture(t, 6)
	read := func(name string) []byte {
		b, err := os.ReadFile("../../pkg/argoworkflow/testdata/" + name)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	e := &projectedVMIEngine{groupEngine: f.engine, source: read("vmi-run-projection.yaml"), fragments: map[argoworkflow.FragmentKey]argoworkflow.Fragment{}}
	for _, name := range []string{"toolchain", "driver"} {
		key := argoworkflow.FragmentKey{Repo: "example/" + name, Version: "v1"}
		e.fragments[key] = argoworkflow.Fragment{Key: key, SHA: strings.Repeat("d", 40), Source: read("vmi-run-" + name + "-fragment.yaml")}
	}
	f.scheduler.jobs = e
	first := f.branch(t, f.terraform, "first", "a")
	second := f.branch(t, f.terraform, "second", "b")
	third := f.branch(t, f.terraform, "third", "c")
	f.start(t)
	created := f.awaitCreated(t, 2)
	if !created[first.ID] || !created[second.ID] {
		t.Fatalf("two two-VM runs did not fit: %v", created)
	}
	f.awaitAdmission(t, third, "KubeVirt capacity", func(o *model.AdmissionObservation) bool { return o != nil && o.KubeVirtCapacity })
	f.engine.finish(t, first.ID)
	created = f.awaitCreated(t, 1)
	if !created[third.ID] {
		t.Fatalf("third run did not follow released peak: %v", created)
	}
	f.engine.assertNoViolations(t)
}
