package service

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
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

func TestPressureZeroStartDoesNotExpire(t *testing.T) {
	for _, tc := range []struct {
		name     string
		since    func(time.Time) time.Time
		deferred bool
	}{
		{"not-started", func(time.Time) time.Time { return time.Time{} }, true},
		{"before-boundary", func(now time.Time) time.Time { return now.Add(-20*time.Minute + time.Nanosecond) }, true},
		{"at-boundary", func(now time.Time) time.Time { return now.Add(-20 * time.Minute) }, false},
		{"after-boundary", func(now time.Time) time.Time { return now.Add(-21 * time.Minute) }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := pressureTestGate()
			f.gate.pressure.state.Holding = true
			r := &admissionReservation{runID: "waiting", pressureSince: tc.since(f.now)}
			f.gate.mu.Lock()
			defer f.gate.mu.Unlock()
			if got := f.gate.deferPressureLocked(r, f.now); got != tc.deferred {
				t.Fatalf("deferred=%t, want %t", got, tc.deferred)
			}
			if tc.deferred && (f.gate.pressure.bypasses != 0 || !r.pressureDeferred) {
				t.Fatal("unexpired waiter bypassed pressure")
			}
			if !tc.deferred && f.gate.pressure.bypasses != 1 {
				t.Fatal("expired waiter did not record its bypass")
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
	if !head.granted || small.granted {
		t.Fatal("expired per-run pressure wait did not preserve XL drain ordering")
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
	if got := service.scheduling(t.Context(), run, nil); got.Admission.Pressure == nil || !got.Admission.Pressure.Redacted || !strings.Contains(got.Message, "host io pressure") || strings.Contains(got.Message, "%") {
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
		if err := os.WriteFile(filepath.Join(root, name), []byte(func() string {
			if name == "cpu" {
				return psiRows("10.25", "")
			}
			return valid
		}()), 0600); err != nil {
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

func TestPressureEachQueuedRunHasOwnMaximumWait(t *testing.T) {
	f := pressureTestGate()
	f.gate.capacity = 16
	f.sample.IO.FullAvg60 = 40
	var order []int64
	seen := map[int64]bool{}
	f.gate.onGrant = func() {
		for id, r := range f.gate.reservations {
			if r.granted && !seen[id] {
				seen[id] = true
				order = append(order, id)
			}
		}
	}
	runs := []*admissionReservation{f.prepare(1, model.PriorityNormal, "", periapsis.S), f.prepare(2, model.PriorityHighest, "", periapsis.S), f.prepare(3, model.PriorityHigh, "", periapsis.S), f.prepare(4, model.PriorityNormal, "", periapsis.S)}
	if state := (&Scheduler{admission: f.gate}).AdmissionState().Pressure; state.Deferred != 4 {
		t.Fatal("not all queued pressure waiters counted", state)
	}
	for _, r := range runs {
		if !r.pressureSince.Equal(f.now) {
			t.Fatal("tail did not start its own clock", r.sequence)
		}
	}
	f.sample.IO.FullAvg60 = 25 // Remains latched, so only maxDefer can grant these.
	f.advance(20*time.Minute - time.Second)
	if len(order) != 0 {
		t.Fatal("granted before maximum wait", order)
	}
	f.advance(time.Second)
	if !reflect.DeepEqual(order, []int64{2, 3, 1, 4}) {
		t.Fatal("four runs did not all grant at maxDefer in priority/FIFO order", order)
	}
	if f.gate.pressure.bypasses != 4 || !f.gate.pressure.state.Holding {
		t.Fatal("recovery rather than per-run expiry admitted the wave")
	}
}

func TestPressureSourceWarnsOncePerOutageAndMetricsAreTyped(t *testing.T) {
	f := pressureTestGate()
	f.err = errors.New("first outage")
	f.advance(time.Second)
	f.advance(time.Second)
	f.err = nil
	f.advance(time.Second)
	f.err = errors.New("second outage")
	f.advance(time.Second)
	f.advance(time.Second)
	warnings := 0
	for _, line := range f.logs {
		if strings.Contains(line, "PSI unavailable") {
			warnings++
		}
	}
	if warnings != 2 {
		t.Fatal("recovery failed to re-arm warning", f.logs)
	}
	var metrics bytes.Buffer
	(&Scheduler{admission: f.gate}).WritePressureMetrics(&metrics)
	for _, want := range []string{"# TYPE oberth_admission_pressure_avg60 gauge", "# TYPE oberth_admission_pressure_deferrals_total counter", "# TYPE oberth_admission_pressure_bypasses_total counter"} {
		if !strings.Contains(metrics.String(), want) {
			t.Fatal("missing type", want)
		}
	}
}

func TestPressureNonAdminAndMCPViewsKeepNonNumericReason(t *testing.T) {
	fixture := newControlFixture(t)
	source, err := fixture.scheduler.EnqueueCI(t.Context(), CIRequest{EventID: "pressure", Repository: fixture.repo, Branch: "feature/pressure", SHA: strings.Repeat("a", 40), Actor: "pusher"})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := fixture.store.ClaimNextRun(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	f := pressureTestGate()
	f.sample.IO.FullAvg60 = 67.25
	r := f.prepare(claimed.QueueSequence, model.PriorityNormal, "", periapsis.M)
	r.runID = source.ID
	r.repoID = fixture.repo.ID
	fixture.scheduler.admission = f.gate
	control := fixture.api(t)
	control.admission = fixture.scheduler
	for _, admin := range []bool{false, true} {
		actor := api.Actor{Identity: "viewer@host", Admin: admin}
		view, err := control.forActor(t.Context(), actor)
		if err != nil {
			t.Fatal(err)
		}
		run := claimed
		run.JobName = "reserved"
		observed := view.scheduling(t.Context(), run, nil)
		if admin {
			if !strings.Contains(observed.Message, "67.2%") {
				t.Fatal("admin HTTP lost detail", observed.Message)
			}
		} else if !strings.Contains(observed.Message, "host io pressure") || strings.Contains(observed.Message, "%") {
			t.Fatal("non-admin reason numeric or misleading", observed.Message)
		}
		value, err := control.CallTool(t.Context(), actor, "run_get", json.RawMessage(`{"id":"`+source.ID+`"}`))
		if err != nil {
			t.Fatal(err)
		}
		body, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Contains(body, []byte("host io pressure")) || bytes.Contains(body, []byte("67.25")) || bytes.Contains(body, []byte(`"avg60"`)) {
			t.Fatalf("MCP reason missing or numeric: %s", body)
		}
	}
}

func TestPressureObserveHighLoadKeepsOtherAdmissionGates(t *testing.T) {
	f := pressureTestGate()
	f.gate.pressure.config.Mode = PressureObserve
	f.gate.pressure.state.Mode = PressureObserve
	f.sample.IO.FullAvg60 = 72
	first := f.prepare(1, model.PriorityNormal, "", periapsis.L)
	second := f.prepare(2, model.PriorityNormal, "", periapsis.L)
	if !first.granted || second.granted { // L weighs 3; a budget of 4 still binds.
		t.Fatal("observation bypassed weighted capacity or blocked the first grant")
	}
	state := (&Scheduler{admission: f.gate}).AdmissionState().Pressure
	if state == nil || state.Mode != PressureObserve || !state.Available || !state.ThresholdExceeded || state.Holding || state.Sample.IO.FullAvg60 != 72 || state.Deferred != 0 {
		t.Fatalf("high pressure must be visible without a hold: %+v", state)
	}
	f.advance(21 * time.Minute)
	if second.granted || !second.pressureSince.IsZero() || f.gate.pressure.deferrals != 0 || f.gate.pressure.bypasses != 0 {
		t.Fatal("observation acquired a deferral clock or bypassed capacity")
	}
	first.cancel()
	if !second.granted {
		t.Fatal("capacity release did not progress under high PSI")
	}
	var metrics bytes.Buffer
	(&Scheduler{admission: f.gate}).WritePressureMetrics(&metrics)
	for _, want := range []string{"oberth_admission_pressure_enforcing 0", "oberth_admission_pressure_threshold_exceeded 1", "oberth_admission_pressure_holding 0"} {
		if !strings.Contains(metrics.String(), want) {
			t.Fatal("missing observation metric", want)
		}
	}
	f.err = errors.New("PSI unavailable")
	f.advance(time.Second)
	state = (&Scheduler{admission: f.gate}).AdmissionState().Pressure
	if state.Available || state.Holding || state.ThresholdExceeded || state.Sample != nil {
		t.Fatal("unavailable PSI must be explicitly unknown", state)
	}
	f.err = nil
	f.sample.IO.FullAvg60 = 15
	f.advance(time.Second)
	state = (&Scheduler{admission: f.gate}).AdmissionState().Pressure
	if !state.Available || state.ThresholdExceeded || state.Holding {
		t.Fatal("observation did not recover in place", state)
	}
}

func TestPressureExplicitEnforcementRecoversWithoutRestart(t *testing.T) {
	f := pressureTestGate()
	f.gate.pressure.config.Mode = PressureEnforce
	f.sample.IO.FullAvg60 = 65
	waiting := f.prepare(1, model.PriorityNormal, "", periapsis.M)
	if waiting.granted || !f.gate.pressure.state.Holding {
		t.Fatal("explicit enforcement failed to hold at loaded-host pressure")
	}
	f.sample.IO.FullAvg60 = 20
	f.advance(time.Second)
	if waiting.granted {
		t.Fatal("enforcement lost its hysteresis")
	}
	f.sample.IO.FullAvg60 = 19.99
	f.advance(time.Second)
	if !waiting.granted || f.gate.pressure.state.Holding || f.gate.pressure.state.ThresholdExceeded {
		t.Fatal("enforcement did not resume without a restart")
	}
	if err := (AdmissionPressureConfig{Mode: "typo"}).Validate(); err == nil {
		t.Fatal("invalid mode disabled protection silently")
	}
}

func TestPressureObserveDoesNotBypassDiskOrGroupOwnership(t *testing.T) {
	f := pressureTestGate()
	f.gate.pressure.config.Mode = PressureObserve
	f.gate.pressure.state.Mode = PressureObserve
	f.sample.IO.FullAvg60 = 70
	f.gate.diskFloor = &diskFloorCheck{floorBytes: 100, cachedFree: 0, cacheTime: time.Now(), cacheTTL: time.Hour}
	waiting := f.prepare(1, model.PriorityNormal, "deploy", periapsis.S)
	if waiting.granted {
		t.Fatal("observation waived the disk floor")
	}
	f.gate.diskFloor = nil
	f.advance(time.Second)
	if !waiting.granted {
		t.Fatal("disk recovery did not progress")
	}
	follower := f.prepare(2, model.PriorityNormal, "deploy", periapsis.S)
	if follower.granted {
		t.Fatal("observation waived group ownership")
	}
	waiting.cancel()
	if !follower.granted {
		t.Fatal("group release did not progress")
	}
}
