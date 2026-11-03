package service

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/pkg/periapsis"
)

func hostPressureTestGate() *pressureFixture {
	f := pressureTestGate()
	c := DefaultAdmissionPressureConfig()
	c.Source = func() (model.PressureSample, error) { f.reads++; return f.sample, f.err }
	c.Logf = func(string, ...any) {}
	f.gate.pressure = newPressureCheck(c)
	return f
}

func TestPressureStarvationDoesNotCrossHardLimits(t *testing.T) {
	for _, tc := range []struct {
		name     string
		cpu, io  float64
		admitted bool
	}{
		{"below hard limits", 49.99, 39.99, true},
		{"CPU hard boundary", 50, 30, false},
		{"IO hard boundary", 45, 40, false},
		{"both extreme", 90, 90, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := hostPressureTestGate()
			f.sample.CPU.SomeAvg10, f.sample.IO.SomeAvg10 = tc.cpu, tc.io
			r := f.prepare(1, model.PriorityNormal, "", periapsis.M)
			f.advance(20 * time.Minute)
			if r.granted {
				t.Fatal("starvation bypass before maxWait elapsed")
			}
			f.advance(time.Second)
			if r.granted != tc.admitted {
				t.Fatalf("grant=%t want %t", r.granted, tc.admitted)
			}
		})
	}
}

func TestHostPressureAvg10AndIndependentHysteresis(t *testing.T) {
	for _, tc := range []struct {
		name    string
		cpu, io float64
		blocked bool
	}{
		{"room", 29.99, 19.99, false}, {"cpu boundary", 30, 0, true}, {"io boundary", 0, 20, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := hostPressureTestGate()
			f.sample.CPU.SomeAvg10, f.sample.IO.SomeAvg10 = tc.cpu, tc.io
			// avg60 must never select or override the avg10 gate.
			f.sample.CPU.SomeAvg60, f.sample.IO.FullAvg60 = 99, 99
			r := f.prepare(1, model.PriorityNormal, "", periapsis.M)
			if r.granted == tc.blocked {
				t.Fatalf("grant=%t blocked=%t", r.granted, tc.blocked)
			}
		})
	}
	f := hostPressureTestGate()
	f.sample.CPU.SomeAvg10, f.sample.IO.SomeAvg10 = 30, 20
	r := f.prepare(1, model.PriorityNormal, "", periapsis.M)
	for _, values := range [][2]float64{{20, 10}, {19.99, 20}, {30, 9.99}, {20, 9.99}} {
		f.sample.CPU.SomeAvg10, f.sample.IO.SomeAvg10 = values[0], values[1]
		f.advance(time.Second)
		if r.granted {
			t.Fatal("one resource cleared the other's hysteresis latch", values)
		}
	}
	f.sample.CPU.SomeAvg10, f.sample.IO.SomeAvg10 = 19.99, 9.99
	f.advance(time.Second)
	if !r.granted {
		t.Fatal("did not recover below both hysteresis boundaries")
	}
}

func TestHostPressureIdleBypassUsesRunClassification(t *testing.T) {
	for _, tc := range []struct {
		name                                    string
		tag, release, promotion, running, grant bool
	}{
		{name: "tag", tag: true, grant: true}, {name: "release", release: true, grant: true},
		{name: "branch"}, {name: "promotion", promotion: true},
		{name: "promotion tag", tag: true, promotion: true},
		{name: "promotion release", release: true, promotion: true},
		{name: "busy tag", tag: true, running: true}, {name: "busy release", release: true, running: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := hostPressureTestGate()
			if tc.running {
				f.prepare(1, model.PriorityNormal, "", periapsis.S)
			}
			f.sample.CPU.SomeAvg10, f.sample.IO.SomeAvg10 = 99, 99
			f.advance(time.Second)
			run := model.Run{ID: "bypass", QueuedAt: f.now, Priority: model.PriorityHighest, Release: tc.release, RefKind: model.RefBranch}
			if tc.tag {
				run.RefKind = model.RefTag
			}
			if tc.promotion {
				run.Trigger = "promotion"
			}
			r := f.gate.reservePriorityRun(2, 2, run)
			f.gate.mu.Lock()
			r.prepared, r.preparedAt, r.size, r.weight = true, f.now, periapsis.S, 1
			f.gate.grantLocked()
			f.gate.mu.Unlock()
			if r.granted != tc.grant {
				t.Fatalf("grant=%t want %t", r.granted, tc.grant)
			}
		})
	}
}

func TestHostPressureUnavailableBudgetOnlyAndRecovery(t *testing.T) {
	f := hostPressureTestGate()
	f.err = errors.New("PSI unavailable")
	r := f.prepare(1, model.PriorityNormal, "", periapsis.M)
	state := (&Scheduler{admission: f.gate}).AdmissionState()
	if !r.granted || state.Pressure.Available || !strings.Contains(state.Pressure.Fallback, "budget-only") {
		t.Fatalf("fallback missing: %+v", state.Pressure)
	}
	f.err = nil
	f.sample.CPU.SomeAvg10 = 60
	f.advance(time.Second)
	waiting := f.prepare(2, model.PriorityNormal, "", periapsis.M)
	if waiting.granted {
		t.Fatal("source recovery did not restore gate")
	}
	observed := (&Scheduler{admission: f.gate}).AdmissionSnapshot(2)
	if observed.Pressure == nil || !strings.Contains(waitingAdmissionMessage(observed), "cpu 60.0%") {
		t.Fatalf("missing pressure numbers: %+v", observed)
	}
	var metrics bytes.Buffer
	(&Scheduler{admission: f.gate}).WritePressureMetrics(&metrics)
	if !strings.Contains(metrics.String(), `oberth_admission_pressure_avg10{resource="cpu",kind="some"} 60`) {
		t.Fatal(metrics.String())
	}
}

func TestHostPressureStarvationOnlyHeadAndBudgetStillApply(t *testing.T) {
	f := hostPressureTestGate()
	f.sample.CPU.SomeAvg10 = 45
	head := f.prepare(1, model.PriorityHighest, "", periapsis.M)
	behind := f.prepare(2, model.PriorityNormal, "", periapsis.M)
	behind.queuedAt = f.now.Add(-time.Hour / 2)
	f.advance(time.Second)
	if head.granted || behind.granted {
		t.Fatal("expired follower bypassed fresh head")
	}
	// Once the head is old, its grant still consumes capacity.
	head.pressureSince = f.now.Add(-21 * time.Minute)
	behind.weight = f.gate.capacity
	f.advance(time.Second)
	if !head.granted || behind.granted {
		t.Fatal("starvation bypassed budget")
	}
}

func TestHostPressureConfigurationAndSampleValidation(t *testing.T) {
	for _, mutate := range []func(*AdmissionPressureConfig){
		func(c *AdmissionPressureConfig) { c.CPUSomeAvg10 = math.NaN() },
		func(c *AdmissionPressureConfig) { c.IOSomeAvg10 = 10 },
		func(c *AdmissionPressureConfig) { c.HardCPU = 29 },
		func(c *AdmissionPressureConfig) { c.HardIO = math.Inf(1) },
		func(c *AdmissionPressureConfig) { c.MaxWaitSeconds = 0 },
	} {
		c := DefaultAdmissionPressureConfig()
		mutate(&c)
		if c.Validate() == nil {
			t.Fatalf("invalid config accepted: %+v", c)
		}
	}
	f := hostPressureTestGate()
	f.sample.CPU.SomeAvg10 = math.NaN()
	if r := f.prepare(1, model.PriorityNormal, "", periapsis.M); !r.granted || f.gate.pressure.state.Available {
		t.Fatal("invalid avg10 did not fail open")
	}
	value, err := parsePressure("some avg10=41.25 avg60=30.5 avg300=1 total=10\n", false)
	if err != nil || value.SomeAvg10 != 41.25 || value.SomeAvg60 != 30.5 {
		t.Fatal(value, err)
	}
}

func TestHostPressureRequiresOnlyCPUAndIOSomeRows(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"cpu", "io"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("some avg10=20 avg60=10 avg300=1 total=5\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	sample, err := readPressureSources(root, false)
	if err != nil || sample.CPU.SomeAvg10 != 20 || sample.IO.SomeAvg60 != 10 {
		t.Fatal(sample, err)
	}
	if err := os.WriteFile(filepath.Join(root, "memory"), []byte("unavailable"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readPressureSources(root, false); err != nil {
		t.Fatal("diagnostic memory must not disable CPU/IO protection", err)
	}
	if err := os.WriteFile(filepath.Join(root, "cpu"), []byte("bad"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readPressureSources(root, false); err == nil {
		t.Fatal("malformed required CPU source accepted")
	}
}

func TestHostPressureEnabledOverridesLegacyObserve(t *testing.T) {
	f := hostPressureTestGate()
	c := f.gate.pressure.config
	c.Mode = PressureObserve
	f.gate.pressure = newPressureCheck(c)
	f.sample.CPU.SomeAvg10 = 60
	if r := f.prepare(1, model.PriorityNormal, "", periapsis.M); r.granted || !f.gate.pressure.state.Holding || f.gate.pressure.state.Mode != PressureEnforce {
		t.Fatal("legacy observe disabled enabled host admission")
	}
}

// Stable PSI models the kernel/startup delay instead of reacting to gate.runs.
func TestHostPressureModerateAdmitsOnePerSettleWindow(t *testing.T) {
	for _, values := range [][2]float64{{20, 0}, {15, 0}, {0, 10}} {
		f := hostPressureTestGate()
		f.sample.CPU.SomeAvg10, f.sample.IO.SomeAvg10 = values[0], values[1]
		first := f.prepare(1, model.PriorityNormal, "", periapsis.S)
		second := f.prepare(2, model.PriorityNormal, "", periapsis.S)
		third := f.prepare(3, model.PriorityNormal, "", periapsis.S)
		if !first.granted || second.granted || third.granted {
			t.Fatalf("moderate PSI %v admitted a burst: runs=%d", values, f.gate.runs)
		}
		f.advance(15 * time.Second)
		f.advance(15*time.Second - time.Nanosecond)
		if second.granted || third.granted {
			t.Fatal("grant before a sample at least 30 seconds after the last grant")
		}
		f.advance(time.Nanosecond)
		if !second.granted || third.granted {
			t.Fatal("settled sample must grant exactly one run")
		}
		f.advance(30 * time.Second)
		if !third.granted {
			t.Fatal("next settle window did not progress")
		}
	}
}

func TestHostPressureIdleAdmitsBackToBack(t *testing.T) {
	f := hostPressureTestGate()
	f.sample.CPU.SomeAvg10, f.sample.IO.SomeAvg10 = 14.99, 9.99
	for id := int64(1); id <= 4; id++ {
		if !f.prepare(id, model.PriorityNormal, "", periapsis.S).granted {
			t.Fatal("idle host did not clear immediately")
		}
	}
}

func TestHostPressureStarvationStartsAtPressureDeferral(t *testing.T) {
	f := hostPressureTestGate()
	// The old queue time may represent a long wait for budget or a VM.
	f.sample.CPU.SomeAvg10 = 45
	r := f.prepare(1, model.PriorityNormal, "", periapsis.S)
	r.queuedAt = f.now.Add(-time.Hour)
	f.advance(time.Second)
	if r.granted {
		t.Fatal("queue age incorrectly counted as pressure starvation")
	}
	// Keep the pressure clock across a brief hysteresis recovery while
	// capacity prevents a grant, so a short dip cannot reset starvation.
	f.gate.maxRuns = 1
	f.gate.runs = 1
	f.sample.CPU.SomeAvg10 = 0
	f.advance(time.Second)
	f.sample.CPU.SomeAvg10 = 45
	f.advance(time.Second)
	f.gate.runs = 0
	f.advance(20*time.Minute - 3*time.Second)
	if r.granted {
		t.Fatal("starvation granted at the exact wait boundary")
	}
	f.advance(time.Second)
	if !r.granted {
		t.Fatal("first pressure deferral was lost across a brief recovery")
	}
}

func TestHostPressureStarvationOnePerPassAndSettleWindow(t *testing.T) {
	f := hostPressureTestGate()
	f.sample.CPU.SomeAvg10 = 45
	first := f.prepare(1, model.PriorityNormal, "", periapsis.S)
	second := f.prepare(2, model.PriorityNormal, "", periapsis.S)
	third := f.prepare(3, model.PriorityNormal, "", periapsis.S)
	// Model waiters previously deferred at the head before priority changed.
	second.pressureSince, third.pressureSince = f.now, f.now
	f.advance(20*time.Minute + time.Second)
	if !first.granted || second.granted || third.granted {
		t.Fatalf("starvation admitted a backlog in one pass: runs=%d", f.gate.runs)
	}
	f.advance(15 * time.Second)
	if second.granted {
		t.Fatal("starvation admitted again before settling")
	}
	f.advance(15 * time.Second)
	if !second.granted || third.granted {
		t.Fatal("starvation must admit only one run in the next window")
	}
}

func TestHostPressureLegacyObserveStartupWarning(t *testing.T) {
	for _, mode := range []string{"", PressureEnforce, PressureObserve} {
		c := DefaultAdmissionPressureConfig()
		c.Mode = mode
		var logs bytes.Buffer
		c.Logf = func(format string, args ...any) { fmt.Fprintf(&logs, format, args...) }
		newPressureCheck(c)
		if warned := strings.Contains(logs.String(), "observe"); warned != (mode == PressureObserve) {
			t.Fatalf("mode=%q warning=%q", mode, logs.String())
		}
	}
}

func TestHostPressureStarvationOnlyOnceInLongPass(t *testing.T) {
	f := hostPressureTestGate()
	f.sample.CPU.SomeAvg10 = 45
	first := f.prepare(1, model.PriorityNormal, "", periapsis.S)
	second := f.prepare(2, model.PriorityNormal, "", periapsis.S)
	first.pressureSince, second.pressureSince = f.now.Add(-21*time.Minute), f.now.Add(-21*time.Minute)
	// A slow grant makes this one pass last longer than the settle window.
	// Per-pass protection must still stop the second starvation grant.
	f.gate.now = func() time.Time {
		f.now = f.now.Add(31 * time.Second)
		return f.now
	}
	f.advance(0)
	if !first.granted || second.granted {
		t.Fatal("a long pass admitted more than one starvation grant")
	}
}

func TestHostPressureBudgetWaitDoesNotStartStarvation(t *testing.T) {
	f := hostPressureTestGate()
	running := f.prepare(1, model.PriorityNormal, "", periapsis.XL)
	f.sample.CPU.SomeAvg10 = 45
	waiting := f.prepare(2, model.PriorityNormal, "", periapsis.S)
	f.advance(21 * time.Minute)
	if !waiting.pressureSince.IsZero() {
		t.Fatal("capacity wait incorrectly started pressure starvation")
	}
	running.cancel()
	if waiting.granted || waiting.pressureSince.IsZero() {
		t.Fatal("releasing capacity should start a pressure deferral")
	}
}
