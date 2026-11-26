package sourcedelivery

import (
	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/sourcecontract"
	core "k8s.io/api/core/v1"
	"strings"
	"testing"
)

func TestResumeLoadServesOwner(t *testing.T) {
	t.Parallel()
	descriptor := sourcecontract.Descriptor{Version: 1, Kind: sourcecontract.Main}
	fix := strings.Repeat("f", 40)
	run := Run{Locator: "r1", Namespace: "runs", Workflow: "shared", UID: "uid", Admission: admissioncontract.PreparedView{RunID: "r2", Digest: "sealed", SourceBindings: []sourcecontract.Binding{{Descriptor: descriptor, Revision: sourcecontract.Revision{Repository: "origin/team/repo", Commit: fix}}}}}
	caller := Caller{Namespace: "runs", Workflow: "shared", UID: "uid", Pod: &core.Pod{Status: core.PodStatus{Phase: core.PodRunning}}}
	request := LoadRequest{Locator: "r1", Descriptor: descriptor}
	b, _, err := selectBinding(run, caller, request)
	if err != nil || b.Revision.Commit != fix {
		t.Fatal("owner did not receive F", b, err)
	}
	caller.Pod.Status.Phase = core.PodSucceeded
	if _, _, err = selectBinding(run, caller, request); err == nil {
		t.Fatal("carried Pod received F")
	}
	caller.Pod.Status.Phase = core.PodRunning
	caller.UID = "replacement"
	if _, _, err = selectBinding(run, caller, request); err == nil {
		t.Fatal("replacement Workflow received F")
	}
}

func TestRerunOfResumedRunIsOrdinary(t *testing.T) {
	t.Parallel()
	d := sourcecontract.Descriptor{Version: 1, Kind: sourcecontract.Main}
	run := Run{Locator: "rf", Namespace: "runs", Workflow: "fresh", UID: "fresh-uid", Admission: admissioncontract.PreparedView{RunID: "rf", Digest: "sealed", SourceBindings: []sourcecontract.Binding{{Descriptor: d}}}}
	c := Caller{Namespace: "runs", Workflow: "fresh", UID: "fresh-uid"}
	if _, _, err := selectBinding(run, c, LoadRequest{Locator: "rf", Descriptor: d}); err != nil {
		t.Fatal("fresh locator refused", err)
	}
	if _, _, err := selectBinding(run, c, LoadRequest{Locator: "r1", Descriptor: d}); err == nil {
		t.Fatal("fresh rerun kept predecessor locator")
	}
}
