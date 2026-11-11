package store

import (
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

func TestNodeBootObservationPersistsExactPodBaseline(t *testing.T) {
	now := time.Now().UTC()
	s := testStore(t, &now)
	repo := createRepo(t, s)
	run, err := s.EnqueueRun(t.Context(), testRunSpec(repo.ID, "boot", strings.Repeat("a", 40)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ClaimNextRun(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err = s.SetRunWorkflowName(t.Context(), run.ID, "workflow"); err != nil {
		t.Fatal(err)
	}
	o := model.NodeBootObservation{RunID: run.ID, WorkflowName: "workflow", WorkflowUID: "11111111-1111-4111-8111-111111111111", NodeID: "workflow-123", PodUID: "22222222-2222-4222-8222-222222222222", NodeName: "worker", NodeUID: "33333333-3333-4333-8333-333333333333", BootID: "44444444-4444-4444-8444-444444444444", StartedAt: now.Add(-time.Minute), ObservedAt: now}
	if reboot, err := s.ObserveNodeBoot(t.Context(), o, true); err != nil || reboot != nil {
		t.Fatalf("baseline: %+v %v", reboot, err)
	}
	var count int
	if err := s.db.QueryRow(`SELECT count(*) FROM node_boot_observations`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("baseline not committed: %d %v", count, err)
	}
	if reboot, err := s.ObserveNodeBoot(t.Context(), o, true); err != nil || reboot != nil {
		t.Fatalf("same boot: %+v %v", reboot, err)
	}
	o.BootID = "55555555-5555-4555-8555-555555555555"
	o.ObservedAt = now.Add(time.Minute)
	for _, field := range []string{"workflow", "pod", "node", "start"} {
		bad := o
		switch field {
		case "workflow":
			bad.WorkflowUID = "66666666-6666-4666-8666-666666666666"
		case "pod":
			bad.PodUID = "66666666-6666-4666-8666-666666666666"
		case "node":
			bad.NodeUID = "66666666-6666-4666-8666-666666666666"
		case "start":
			bad.StartedAt = bad.StartedAt.Add(time.Second)
		}
		if reboot, err := s.ObserveNodeBoot(t.Context(), bad, false); err != nil || reboot != nil {
			t.Fatalf("%s reassigned evidence: %+v %v", field, reboot, err)
		}
	}
	reboot, err := s.ObserveNodeBoot(t.Context(), o, false)
	if err != nil || reboot == nil || !reboot.After.Equal(now) || !reboot.ObservedAt.Equal(o.ObservedAt) {
		t.Fatalf("changed boot: %+v %v", reboot, err)
	}
	o.ObservedAt = o.ObservedAt.Add(time.Minute)
	again, err := s.ObserveNodeBoot(t.Context(), o, false)
	if err != nil || again == nil || !again.ObservedAt.Equal(reboot.ObservedAt) {
		t.Fatalf("discovery time rewritten: %+v %v", again, err)
	}
}
