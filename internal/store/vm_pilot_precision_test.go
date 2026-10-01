package store

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/oberthci/oberth/internal/vmrunner"
)

func TestPilotConductorObservedTimestampPrecision(t *testing.T) {
	base := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
	for _, mode := range []string{"serialized-same-second", "serialized-earlier-second", "precise-before-admission", "precise-future"} {
		t.Run(mode, func(t *testing.T) {
			f := newPilotFixture(t)
			*f.now = base.Add(250 * time.Millisecond)
			f.start(t)
			*f.now = base.Add(750 * time.Millisecond)
			f.create(t, vmrunner.ConductorResource, "Job")
			pod := f.pod(t, "pod-conductor", vmrunner.ConductorResource)
			started := base.Add(500 * time.Millisecond)
			if mode == "serialized-earlier-second" {
				started = base.Add(-500 * time.Millisecond)
			}
			if strings.HasPrefix(mode, "serialized-") {
				body, err := json.Marshal(metav1.NewTime(started))
				mustPilot(t, err)
				var observed metav1.Time
				mustPilot(t, json.Unmarshal(body, &observed))
				started = observed.Time
				if started.Nanosecond() != 0 {
					t.Fatal("metav1 serialization did not exercise precision loss")
				}
			}
			if mode == "precise-before-admission" {
				started = base.Add(100 * time.Millisecond)
			}
			if mode == "precise-future" {
				started = base.Add(900 * time.Millisecond)
			}
			attempt := vmrunner.ConductorAttempt{AttemptID: "observed", JobUID: "conductor-uid", PodUID: pod.UID, Container: "conductor", ContainerID: "containerd://observed", ImageDigest: "sha256:" + strings.Repeat("e", 64), SpecIdentity: pod.SpecIdentity, StartedAt: started}
			err := f.s.BindConductorAttempt(context.Background(), f.plan.Spec.RunID, attempt)
			if mode != "serialized-same-second" {
				if err == nil {
					t.Fatal("out-of-bounds timestamp accepted")
				}
				return
			}
			mustPilot(t, err)
			state, err := f.s.PilotExecution(context.Background(), f.plan.Spec.RunID)
			mustPilot(t, err)
			if state.Attempt == nil || !state.Attempt.StartedAt.Equal(base) || !state.CreatedAt.Equal(base.Add(250*time.Millisecond)) {
				t.Fatal("actual process or durable admission time changed")
			}
			attempt.ContainerID = "containerd://replacement"
			if err := f.s.BindConductorAttempt(context.Background(), f.plan.Spec.RunID, attempt); err == nil {
				t.Fatal("precision repair allowed attempt replacement")
			}
		})
	}
}
