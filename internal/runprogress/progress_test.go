package runprogress

import (
	"encoding/json"
	"testing"
	"time"
)

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
			Version: Version, Stage: "test", Step: "unit", Ordinal: 0,
			Status:    status,
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
		e.Stage = ""
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
		e.Stage = "test\x00burn"
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

func TestPlannedStepSecretsIsAdditive(t *testing.T) {
	cases := []struct {
		name     string
		step     PlannedStep
		wantJSON string
	}{
		{"plain", PlannedStep{Stage: "test", Step: "lint", Ordinal: 0}, `{"burn":"test","step":"lint","ordinal":0,"credentialed":false}`},
		{"secrets", PlannedStep{Stage: "publish", Step: "push", Ordinal: 1, Secrets: true}, `{"burn":"publish","step":"push","ordinal":1,"secrets":true,"credentialed":false}`},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			body, err := json.Marshal(testCase.step)
			if err != nil {
				t.Fatal(err)
			}
			if string(body) != testCase.wantJSON {
				t.Fatalf("encoded %s, want %s", body, testCase.wantJSON)
			}
			var decoded PlannedStep
			if err := json.Unmarshal(body, &decoded); err != nil {
				t.Fatal(err)
			}
			if decoded != testCase.step {
				t.Fatalf("round trip %+v, want %+v", decoded, testCase.step)
			}
		})
	}

}
