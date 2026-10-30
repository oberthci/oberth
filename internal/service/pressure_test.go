package service

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/periapsis"
)

type pressureFixture struct {
	gate   *weightedAdmission
	now    time.Time
	sample model.PressureSample
	err    error
	reads  int
	logs   []string
}

func pressureTestGate() *pressureFixture {
	f := &pressureFixture{now: time.Date(2026, 10, 30, 15, 0, 0, 0, time.UTC)}
	f.gate = newWeightedAdmission(4)
	f.gate.now = func() time.Time { return f.now }
	f.gate.pressure = newPressureCheck(AdmissionPressureConfig{IOFullAvg60Max: 30, Hysteresis: 10, MaxDefer: 20 * time.Minute,
		Source: func() (model.PressureSample, error) { f.reads++; return f.sample, f.err },
		Logf:   func(format string, args ...any) { f.logs = append(f.logs, fmt.Sprintf(format, args...)) },
	})
	return f
}

func (f *pressureFixture) prepare(id int64, priority model.RunPriority, group string, size periapsis.Size) *admissionReservation {
	r := f.gate.reservePriorityRun(id, id, model.Run{ID: fmt.Sprint(id), Priority: priority, QueuedAt: f.now, RepoID: 1})
	f.gate.mu.Lock()
	defer f.gate.mu.Unlock()
	r.prepared, r.preparedAt = true, f.now
	r.size, r.weight = size, f.gate.weights.of(size, f.gate.capacity)
	r.groupName = group
	if group != "" {
		r.group = concurrencyGroupKey(1, group)
	}
	f.gate.grantLocked()
	return r
}

func (f *pressureFixture) advance(d time.Duration) {
	f.gate.mu.Lock()
	defer f.gate.mu.Unlock()
	f.now = f.now.Add(d)
	f.gate.grantLocked()
}

func TestPressureThresholdHysteresisAndNewWorkOnly(t *testing.T) {
	f := pressureTestGate()
	f.sample.IO.FullAvg60 = 29.99
	running := f.prepare(1, model.PriorityNormal, "", periapsis.M)
	if !running.granted {
		t.Fatal("below threshold did not grant")
	}
	f.sample.IO.FullAvg60 = 30
	f.advance(time.Second)
	waiting := f.prepare(2, model.PriorityNormal, "", periapsis.M)
	if waiting.granted || !running.granted || f.gate.used != 2 {
		t.Fatal("pressure must hold new work and leave running grant alone")
	}
	scheduler := &Scheduler{admission: f.gate}
	observation := scheduler.AdmissionSnapshot(2)
	if observation.Pressure == nil || !strings.Contains(waitingAdmissionMessage(observation), "io pressure 30.0%") {
		t.Fatalf("missing pressure reason: %+v", observation)
	}
	if scheduler.AdmissionSnapshot(1).Pressure != nil {
		t.Fatal("running job reported deferred")
	}
	for _, value := range []float64{29, 20} {
		f.sample.IO.FullAvg60 = value
		f.advance(time.Second)
		if waiting.granted {
			t.Fatal("hysteresis released at or above resume boundary", value)
		}
	}
	f.sample.IO.FullAvg60 = 19.99
	f.advance(time.Second)
	if !waiting.granted || scheduler.AdmissionState().Pressure.Holding {
		t.Fatal("pressure did not resume strictly below 20")
	}
	if f.gate.pressure.deferrals != 1 {
		t.Fatal("deferral counted on every poll")
	}
}

func TestPressureCPUGateOptionalAndIndependent(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(fmt.Sprint(enabled), func(t *testing.T) {
			f := pressureTestGate()
			if enabled {
				f.gate.pressure.config.CPUSomeAvg60Max = 60
			}
			f.sample.CPU.SomeAvg60 = 80
			f.sample.Memory.FullAvg60 = 99 // Observed, never a new memory gate.
			r := f.prepare(1, model.PriorityNormal, "", periapsis.M)
			if r.granted == enabled {
				t.Fatal("optional CPU threshold ignored")
			}
			if enabled {
				if f.gate.pressure.state.Resource != "cpu" {
					t.Fatal("wrong pressure resource")
				}
				f.sample.IO.FullAvg60 = 40
				f.sample.CPU.SomeAvg60 = 49
				f.advance(time.Second)
				if r.granted || f.gate.pressure.state.Resource != "io" {
					t.Fatal("CPU recovery bypassed IO hold")
				}
				f.sample.IO.FullAvg60 = 19
				f.advance(time.Second)
				if !r.granted {
					t.Fatal("independent latches did not recover")
				}
			}
		})
	}
}

func TestPressureMaximumDeferralAdmitsButPreservesOtherGates(t *testing.T) {
	for _, guard := range []string{"none", "weight", "whole-budget", "run-ceiling", "disk-floor", "group", "vm"} {
		t.Run(guard, func(t *testing.T) {
			f := pressureTestGate()
			f.sample.IO.FullAvg60 = 80
			size := periapsis.M
			if guard == "whole-budget" {
				size = periapsis.XL
			}
			r := f.prepare(1, model.PriorityNormal, "", size)
			if r.granted {
				t.Fatal("pressure did not hold")
			}
			switch guard {
			case "weight":
				f.gate.used = 4
			case "whole-budget":
				f.gate.used = 1
			case "run-ceiling":
				f.gate.maxRuns = 1
				f.gate.runs = 1
			case "disk-floor":
				f.gate.diskFloor = &diskFloorCheck{floorBytes: 100, cachedFree: 0, cacheTime: time.Now(), cacheTTL: time.Hour}
			case "group":
				r.group = "1/group"
				f.gate.groups[r.group] = &admissionReservation{runID: "owner"}
			case "vm":
				r.vmParked = true
			}
			f.advance(20*time.Minute - time.Nanosecond)
			if r.granted {
				t.Fatal("max defer expired early")
			}
			f.advance(time.Nanosecond)
			if guard == "none" {
				if !r.granted {
					t.Fatal("persistent pressure starved eligible work")
				}
				return
			}
			if r.granted || f.gate.pressure.bypasses != 0 {
				t.Fatal("pressure timeout bypassed", guard)
			}
			f.gate.used = 0
			f.gate.runs = 0
			f.gate.diskFloor = nil
			delete(f.gate.groups, r.group)
			r.vmParked = false
			f.advance(time.Second)
			if !r.granted || f.gate.pressure.bypasses != 1 {
				t.Fatal("guard recovery did not allow bounded pressure bypass", guard)
			}
		})
	}
}

func TestPressureTimeoutDoesNotJumpHigherPriorityOrGroupHead(t *testing.T) {
	f := pressureTestGate()
	f.sample.IO.FullAvg60 = 80
	normal := f.prepare(1, model.PriorityNormal, "deploy", periapsis.M)
	f.advance(19 * time.Minute)
	release := f.prepare(2, model.PriorityHighest, "", periapsis.M)
	tag := f.prepare(3, model.PriorityHigh, "", periapsis.M)
	f.advance(time.Minute)
	if normal.granted || release.granted || tag.granted {
		t.Fatal("expired normal run jumped pressure-held release/tag priority")
	}
	f.sample.IO.FullAvg60 = 0
	f.advance(time.Second)
	if !release.granted || !tag.granted || normal.granted {
		t.Fatal("pressure recovery lost release/tag ordering")
	}
	// Release both higher classes. First group head wins; its follower waits.
	f.gate.mu.Lock()
	f.gate.releaseLocked(release)
	f.gate.releaseLocked(tag)
	f.gate.mu.Unlock()
	follower := f.prepare(4, model.PriorityNormal, "deploy", periapsis.M)
	if !normal.granted || follower.granted {
		t.Fatal("pressure path changed concurrency-group ordering")
	}
	f.gate.mu.Lock()
	f.gate.releaseLocked(normal)
	f.gate.mu.Unlock()
	if !follower.granted {
		t.Fatal("group head release did not wake follower")
	}
}

func TestPressureTimeoutPreservesBackfillDrain(t *testing.T) {
	f := pressureTestGate()
	f.gate.maxHeadWait = time.Minute
	running := f.prepare(1, model.PriorityNormal, "", periapsis.M)
	head := f.prepare(2, model.PriorityNormal, "", periapsis.XL)
	f.sample.IO.FullAvg60 = 80
	f.advance(time.Second)
	small := f.prepare(3, model.PriorityNormal, "", periapsis.S)
	if small.pressureSince.IsZero() {
		t.Fatal("fixture did not reach pressure gate during backfill window")
	}
	f.advance(21 * time.Minute)
	if small.granted || head.granted {
		t.Fatal("pressure timeout bypassed closed backfill window")
	}
	f.gate.mu.Lock()
	f.gate.releaseLocked(running)
	f.gate.mu.Unlock()
	if head.granted {
		t.Fatal("newly eligible XL bypassed its own pressure wait")
	}
	f.sample.IO.FullAvg60 = 0
	f.advance(time.Second)
	if !head.granted || small.granted {
		t.Fatal("recovery did not drain for XL first")
	}
}

func TestPressureUnavailableFailsOpenWarnsOnceAndResetsEpisode(t *testing.T) {
	f := pressureTestGate()
	f.sample.IO.FullAvg60 = 80
	r := f.prepare(1, model.PriorityNormal, "", periapsis.M)
	f.err = errors.New("parse fixture error")
	f.advance(time.Second)
	if !r.granted || f.gate.pressure.state.Available || f.gate.pressure.state.Sample != nil || f.gate.pressure.state.Holding {
		t.Fatal("unavailable PSI did not fail open")
	}
	f.advance(time.Second)
	warnings := 0
	for _, line := range f.logs {
		if strings.Contains(line, "PSI unavailable") {
			warnings++
		}
	}
	if warnings != 1 {
		t.Fatal("unavailable warning was not emitted once", f.logs)
	}
	f.err = nil
	f.gate.capacity = 6
	f.sample.IO.FullAvg60 = 80
	f.advance(time.Second)
	next := f.prepare(2, model.PriorityNormal, "", periapsis.M)
	if next.granted {
		t.Fatal("valid PSI did not resume pressure protection")
	}
	f.advance(19 * time.Minute)
	f.sample.IO.FullAvg60 = 0
	f.advance(time.Second)
	// A fresh episode must not inherit a previous run's pressure timeout.
	f.sample.IO.FullAvg60 = 80
	f.advance(time.Second)
	third := f.prepare(3, model.PriorityNormal, "", periapsis.M)
	if third.granted || !third.pressureSince.Equal(f.now) {
		t.Fatal("fresh pressure episode inherited an old timeout")
	}
}

func TestPressureAdoptionPreservesExistingWorkflowGrant(t *testing.T) {
	f := pressureTestGate()
	f.sample.IO.FullAvg60 = 80
	r, err := f.gate.adopt(model.Run{ID: "existing", QueueSequence: 1, RepoID: 1}, periapsis.M, argoworkflow.VMIDemand{})
	if err != nil || !r.granted || !r.submitted {
		t.Fatal("pressure held adoption of running work", err)
	}
	f.advance(time.Second)
	if !r.granted || f.gate.used != 2 || f.gate.pressure.deferrals != 0 {
		t.Fatal("pressure touched adopted work")
	}
}

func TestPressureObservationsMetricsAndScopedRedaction(t *testing.T) {
	f := pressureTestGate()
	f.sample.IO.FullAvg60 = 61.25
	f.sample.CPU.SomeAvg60 = 21
	f.sample.Memory.FullAvg60 = 3
	f.prepare(1, model.PriorityNormal, "", periapsis.M)
	scheduler := &Scheduler{admission: f.gate}
	reads := f.reads
	state := scheduler.AdmissionState()
	if state.Pressure == nil || !state.Pressure.Available || !state.Pressure.Holding || state.Pressure.Deferred != 1 || state.Reservations[0].Pressure == nil {
		t.Fatalf("missing state: %+v", state)
	}
	var metrics bytes.Buffer
	scheduler.WritePressureMetrics(&metrics)
	for _, want := range []string{"oberth_admission_pressure_available 1", "oberth_admission_pressure_holding 1", "oberth_admission_pressure_deferred 1", "oberth_admission_pressure_deferrals_total 1", `resource="io",kind="full"} 61.25`, `resource="memory",kind="full"} 3`} {
		if !strings.Contains(metrics.String(), want) {
			t.Fatal("missing metric", want, metrics.String())
		}
	}
	if f.reads != reads {
		t.Fatal("observation performed host IO")
	}
	state.Pressure.Sample.IO.FullAvg60 = 0
	if scheduler.AdmissionState().Pressure.Sample.IO.FullAvg60 != 61.25 {
		t.Fatal("status caller mutated admission sample")
	}
	service := &API{admission: scheduler}
	run := model.Run{ID: "1", Status: model.RunRunning, QueueSequence: 1, RepoID: 1, JobName: "reserved"}
	if got := service.scheduling(t.Context(), run, nil); !strings.Contains(got.Message, "io pressure 61.2%") {
		t.Fatal("missing run reason", got.Message)
	}
	service.scope = &repoScope{actor: api.Actor{Scope: []string{"forge"}}, repos: map[int64]model.Repository{1: {ID: 1}}} // Visible blocker must still hide host-wide pressure.
	if got := service.scheduling(t.Context(), run, nil); got.Admission.Pressure != nil || strings.Contains(got.Message, "pressure") {
		t.Fatal("scoped run leaked host pressure", got)
	}
}

func psiRows(some, full string) string {
	result := "some avg10=0.01 avg60=" + some + " avg300=0.03 total=100\n"
	if full != "" {
		result += "full avg10=0.00 avg60=" + full + " avg300=0.00 total=10\n"
	}
	return result
}
func TestPressureParsingAndFailOpenReadErrors(t *testing.T) {
	valid := psiRows("10.25", "4.50")
	value, err := parsePressure(valid, true)
	if err != nil || value.SomeAvg60 != 10.25 || value.FullAvg60 != 4.5 {
		t.Fatal("valid PSI rejected", value, err)
	}
	if _, err := parsePressure(psiRows("0", ""), false); err != nil {
		t.Fatal("older CPU PSI requires no full row", err)
	}
	for _, data := range []string{"", psiRows("1", ""), strings.Replace(valid, "10.25", "NaN", 1), strings.Replace(valid, "10.25", "Inf", 1), strings.Replace(valid, "10.25", "101", 1), strings.Replace(valid, "10.25", "-1", 1), strings.Replace(valid, "avg60=10.25", "avg60=10.25 avg60=1", 1), valid + valid, strings.Replace(valid, "total=100", "total=-1", 1), strings.Replace(valid, "avg10=0.01 ", "", 1), "garbage\n"} {
		if _, err := parsePressure(data, true); err == nil {
			t.Fatalf("malformed PSI accepted: %q", data)
		}
	}
	root := t.TempDir()
	for _, name := range []string{"io", "memory", "cpu"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(valid), 0600); err != nil {
			t.Fatal(err)
		}
	}
	sample, err := readPressureDirectory(root)
	if err != nil || sample.IO.FullAvg60 != 4.5 || sample.Memory.SomeAvg60 != 10.25 {
		t.Fatal("directory source lost resource data", sample, err)
	}
	for _, invalid := range []string{"bad format", strings.Repeat("x", (16<<10)+1)} {
		if err := os.WriteFile(filepath.Join(root, "io"), []byte(invalid), 0600); err != nil {
			t.Fatal(err)
		}
		f := pressureTestGate()
		f.gate.pressure.config.Source = func() (model.PressureSample, error) { return readPressureDirectory(root) }
		if r := f.prepare(1, model.PriorityNormal, "", periapsis.M); !r.granted || f.gate.pressure.state.Available {
			t.Fatal("parse/oversize error blocked admission")
		}
	}
	if err := os.WriteFile(filepath.Join(root, "io"), []byte(valid), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "memory")); err != nil {
		t.Fatal(err)
	}
	if _, err := readPressureDirectory(root); err == nil {
		t.Fatal("missing PSI source accepted")
	}
}

func TestPressureValidationAndDisabledSource(t *testing.T) {
	for _, config := range []AdmissionPressureConfig{{IOFullAvg60Max: math.NaN()}, {CPUSomeAvg60Max: math.Inf(1)}, {IOFullAvg60Max: -1}, {IOFullAvg60Max: 101}, {Hysteresis: -1}, {IOFullAvg60Max: 10, Hysteresis: 10, MaxDefer: time.Minute}, {IOFullAvg60Max: 30}, {MaxDefer: -time.Minute}} {
		if config.Validate() == nil {
			t.Fatalf("invalid config accepted: %+v", config)
		}
	}
	disabled := AdmissionPressureConfig{Source: func() (model.PressureSample, error) {
		t.Fatal("disabled pressure read host")
		return model.PressureSample{}, nil
	}}
	if err := disabled.Validate(); err != nil || newPressureCheck(disabled) != nil {
		t.Fatal("disabled config changed legacy behavior", err)
	}
	f := pressureTestGate()
	f.sample.IO.FullAvg60 = math.NaN()
	if r := f.prepare(1, model.PriorityNormal, "", periapsis.M); !r.granted {
		t.Fatal("non-finite injected sample held admission")
	}
}

func TestPressureCachedReadsAreRaceSafe(t *testing.T) {
	f := pressureTestGate()
	f.sample.IO.FullAvg60 = 60
	f.prepare(1, model.PriorityNormal, "", periapsis.M)
	scheduler := &Scheduler{admission: f.gate}
	var workers sync.WaitGroup
	for range 8 {
		workers.Go(func() {
			for range 30 {
				_ = scheduler.AdmissionSnapshot(1)
				_ = scheduler.AdmissionState()
				var metrics bytes.Buffer
				scheduler.WritePressureMetrics(&metrics)
				kick(f.gate)
			}
		})
	}
	workers.Wait()
	if f.reads != 1 || f.gate.pressure.deferrals != 1 {
		t.Fatal("shared cache or per-run counter bypassed", f.reads)
	}
}

// A run can remain queued across recovery because a different capacity gate
// still blocks it. Its next pressure episode must not inherit the old timer.
func TestPressureEpisodeResetsWhileCapacityBlocks(t *testing.T) {
	for _, unavailable := range []bool{false, true} {
		t.Run(fmt.Sprint(unavailable), func(t *testing.T) {
			f := pressureTestGate()
			f.sample.IO.FullAvg60 = 80
			r := f.prepare(1, model.PriorityNormal, "", periapsis.M)
			f.gate.used = f.gate.capacity
			f.advance(19 * time.Minute)
			if unavailable {
				f.err = errors.New("unavailable PSI")
			} else {
				f.sample.IO.FullAvg60 = 0
			}
			f.advance(time.Second)
			if r.granted || !r.pressureSince.IsZero() {
				t.Fatal("recovery retained old pressure episode")
			}
			f.err = nil
			f.sample.IO.FullAvg60 = 80
			f.gate.used = 0
			f.advance(time.Second)
			since := f.now
			if r.granted || !r.pressureSince.Equal(since) {
				t.Fatal("new episode did not begin a fresh wait")
			}
			f.advance(2 * time.Minute)
			if r.granted {
				t.Fatal("old episode prematurely bypassed pressure")
			}
			f.advance(18 * time.Minute)
			if !r.granted {
				t.Fatal("fresh episode did not honor maximum defer")
			}
		})
	}
}
