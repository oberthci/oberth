package runprogress

import (
	"bytes"
	"testing"
	"time"

	"github.com/oberthci/oberth/pkg/periapsis"
)

func TestMarkerRoundTripAndStrictValidation(t *testing.T) {
	started := time.Now().UTC()
	event := Event{
		Version: Version, Burn: "test", Step: "unit", Ordinal: 2,
		Status: StepRunning, DeclaredSize: periapsis.L, StartedAt: &started,
	}
	marker, err := MarshalMarker(event)
	if err != nil {
		t.Fatal(err)
	}
	decoded, found, err := ParseMarker(marker)
	if err != nil || !found || decoded.Burn != event.Burn || decoded.Step != event.Step ||
		decoded.Ordinal != event.Ordinal || decoded.Status != StepRunning || decoded.StartedAt == nil {
		t.Fatalf("decoded marker = %#v, found=%v, err=%v", decoded, found, err)
	}
	if _, found, err := ParseMarker([]byte("[test/unit] ordinary output\n")); err != nil || found {
		t.Fatalf("ordinary output parsed as progress: found=%v err=%v", found, err)
	}
	malformed := append([]byte(MarkerPrefix), []byte(`{"version":1,"burn":"test","step":"unit","ordinal":0,"status":"queued","unknown":true}`)...)
	if _, found, err := ParseMarker(malformed); !found || err == nil {
		t.Fatalf("malformed marker = found=%v err=%v", found, err)
	}
	oversized := append([]byte(MarkerPrefix), bytes.Repeat([]byte("x"), MaxMarkerBytes)...)
	if _, found, err := ParseMarker(oversized); !found || err == nil {
		t.Fatalf("oversized marker = found=%v err=%v", found, err)
	}
}

func TestStatusTerminal(t *testing.T) {
	for _, tc := range []struct {
		status Status
		want   bool
	}{
		{StepQueued, false},
		{StepRunning, false},
		{StepPassed, true},
		{StepFailed, true},
		{StepSkipped, true},
		{StepTimedOut, true},
		{Status("unknown"), false},
		{Status(""), false},
	} {
		if got := tc.status.terminal(); got != tc.want {
			t.Errorf("Status(%q).terminal() = %v, want %v", tc.status, got, tc.want)
		}
	}
}

func TestEventTerminal(t *testing.T) {
	for _, tc := range []struct {
		status Status
		want   bool
	}{
		{StepQueued, false},
		{StepRunning, false},
		{StepPassed, true},
		{StepFailed, true},
		{StepSkipped, true},
		{StepTimedOut, true},
	} {
		event := Event{Status: tc.status}
		if got := event.Terminal(); got != tc.want {
			t.Errorf("Event{Status: %q}.Terminal() = %v, want %v", tc.status, got, tc.want)
		}
	}
}

func TestEventValidate(t *testing.T) {
	now := time.Now().UTC()
	later := now.Add(time.Minute)

	valid := func(status Status, started, finished *time.Time) Event {
		return Event{
			Version: Version, Burn: "test", Step: "unit", Ordinal: 0,
			Status: status, DeclaredSize: periapsis.M,
			StartedAt: started, FinishedAt: finished,
		}
	}

	t.Run("valid queued", func(t *testing.T) {
		if err := valid(StepQueued, nil, nil).Validate(); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("valid running", func(t *testing.T) {
		if err := valid(StepRunning, &now, nil).Validate(); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("valid passed", func(t *testing.T) {
		if err := valid(StepPassed, &now, &later).Validate(); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("valid failed", func(t *testing.T) {
		if err := valid(StepFailed, &now, &later).Validate(); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("valid skipped", func(t *testing.T) {
		if err := valid(StepSkipped, &now, &later).Validate(); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("valid timed_out", func(t *testing.T) {
		if err := valid(StepTimedOut, &now, &later).Validate(); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("wrong version", func(t *testing.T) {
		e := valid(StepQueued, nil, nil)
		e.Version = 99
		if err := e.Validate(); err == nil {
			t.Fatal("accepted wrong version")
		}
	})
	t.Run("empty burn", func(t *testing.T) {
		e := valid(StepQueued, nil, nil)
		e.Burn = ""
		if err := e.Validate(); err == nil {
			t.Fatal("accepted empty burn")
		}
	})
	t.Run("empty step", func(t *testing.T) {
		e := valid(StepQueued, nil, nil)
		e.Step = ""
		if err := e.Validate(); err == nil {
			t.Fatal("accepted empty step")
		}
	})
	t.Run("negative ordinal", func(t *testing.T) {
		e := valid(StepQueued, nil, nil)
		e.Ordinal = -1
		if err := e.Validate(); err == nil {
			t.Fatal("accepted negative ordinal")
		}
	})
	t.Run("invalid declared size", func(t *testing.T) {
		e := valid(StepQueued, nil, nil)
		e.DeclaredSize = periapsis.Size("XXXL")
		if err := e.Validate(); err == nil {
			t.Fatal("accepted invalid declared size")
		}
	})
	t.Run("queued with timestamps", func(t *testing.T) {
		if err := valid(StepQueued, &now, nil).Validate(); err == nil {
			t.Fatal("accepted queued with started_at")
		}
		if err := valid(StepQueued, nil, &later).Validate(); err == nil {
			t.Fatal("accepted queued with finished_at")
		}
	})
	t.Run("running without started_at", func(t *testing.T) {
		if err := valid(StepRunning, nil, nil).Validate(); err == nil {
			t.Fatal("accepted running without started_at")
		}
	})
	t.Run("running with finished_at", func(t *testing.T) {
		if err := valid(StepRunning, &now, &later).Validate(); err == nil {
			t.Fatal("accepted running with finished_at")
		}
	})
	t.Run("terminal without timestamps", func(t *testing.T) {
		if err := valid(StepPassed, nil, nil).Validate(); err == nil {
			t.Fatal("accepted passed without timestamps")
		}
		if err := valid(StepPassed, &now, nil).Validate(); err == nil {
			t.Fatal("accepted passed without finished_at")
		}
	})
	t.Run("finished_at before started_at", func(t *testing.T) {
		if err := valid(StepPassed, &later, &now).Validate(); err == nil {
			t.Fatal("accepted finished_at before started_at")
		}
	})
	t.Run("unknown status", func(t *testing.T) {
		if err := valid(Status("exploded"), &now, &later).Validate(); err == nil {
			t.Fatal("accepted unknown status")
		}
	})
	t.Run("burn with NUL", func(t *testing.T) {
		e := valid(StepQueued, nil, nil)
		e.Burn = "test\x00burn"
		if err := e.Validate(); err == nil {
			t.Fatal("accepted burn with NUL byte")
		}
	})
	t.Run("step with newline", func(t *testing.T) {
		e := valid(StepQueued, nil, nil)
		e.Step = "line\none"
		if err := e.Validate(); err == nil {
			t.Fatal("accepted step with newline")
		}
	})
}

func TestPlanValidate(t *testing.T) {
	t.Run("valid plan", func(t *testing.T) {
		plan := Plan{
			Version: PlanVersion,
			Steps: []PlannedStep{
				{Burn: "test", Step: "lint", Ordinal: 0},
				{Burn: "test", Step: "unit", Ordinal: 1},
				{Burn: "build", Step: "compile", Ordinal: 2},
			},
		}
		if err := plan.Validate(); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("empty steps valid", func(t *testing.T) {
		plan := Plan{Version: PlanVersion, Steps: nil}
		if err := plan.Validate(); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("wrong version", func(t *testing.T) {
		plan := Plan{Version: 99, Steps: nil}
		if err := plan.Validate(); err == nil {
			t.Fatal("accepted wrong plan version")
		}
	})
	t.Run("empty burn", func(t *testing.T) {
		plan := Plan{
			Version: PlanVersion,
			Steps:   []PlannedStep{{Burn: "", Step: "lint", Ordinal: 0}},
		}
		if err := plan.Validate(); err == nil {
			t.Fatal("accepted empty burn in plan step")
		}
	})
	t.Run("empty step", func(t *testing.T) {
		plan := Plan{
			Version: PlanVersion,
			Steps:   []PlannedStep{{Burn: "test", Step: "", Ordinal: 0}},
		}
		if err := plan.Validate(); err == nil {
			t.Fatal("accepted empty step in plan step")
		}
	})
	t.Run("negative ordinal", func(t *testing.T) {
		plan := Plan{
			Version: PlanVersion,
			Steps:   []PlannedStep{{Burn: "test", Step: "lint", Ordinal: -1}},
		}
		if err := plan.Validate(); err == nil {
			t.Fatal("accepted negative ordinal in plan step")
		}
	})
	t.Run("duplicate burn/step key", func(t *testing.T) {
		plan := Plan{
			Version: PlanVersion,
			Steps: []PlannedStep{
				{Burn: "test", Step: "lint", Ordinal: 0},
				{Burn: "test", Step: "lint", Ordinal: 1},
			},
		}
		if err := plan.Validate(); err == nil {
			t.Fatal("accepted duplicate burn/step key")
		}
	})
	t.Run("duplicate ordinal", func(t *testing.T) {
		plan := Plan{
			Version: PlanVersion,
			Steps: []PlannedStep{
				{Burn: "test", Step: "lint", Ordinal: 0},
				{Burn: "test", Step: "unit", Ordinal: 0},
			},
		}
		if err := plan.Validate(); err == nil {
			t.Fatal("accepted duplicate ordinal")
		}
	})
	t.Run("burn with NUL", func(t *testing.T) {
		plan := Plan{
			Version: PlanVersion,
			Steps:   []PlannedStep{{Burn: "test\x00x", Step: "lint", Ordinal: 0}},
		}
		if err := plan.Validate(); err == nil {
			t.Fatal("accepted burn with NUL in plan")
		}
	})
}
