package service

import (
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

const (
	PressureObserve      = "observe"
	PressureEnforce      = "enforce"
	pressureSettleWindow = 30 * time.Second
)

// AdmissionPressureConfig observes host pressure and optionally gates new grants.
// Enabled selects the avg10 host gate. Legacy avg60 fields remain available
// for existing in-process callers; serve and the chart use the avg10 gate.
// Source and Logf permit deterministic tests without observing the test host.
type AdmissionPressureConfig struct {
	Enabled         bool
	CPUSomeAvg10    float64
	IOSomeAvg10     float64
	HardCPU         float64
	HardIO          float64
	MaxWaitSeconds  int64
	Mode            string
	IOFullAvg60Max  float64
	CPUSomeAvg60Max float64
	Hysteresis      float64
	MaxDefer        time.Duration
	Source          func() (model.PressureSample, error)
	Logf            func(string, ...any)
}

func DefaultAdmissionPressureConfig() AdmissionPressureConfig {
	return AdmissionPressureConfig{Enabled: true, Mode: PressureEnforce, CPUSomeAvg10: 40, IOSomeAvg10: 25, HardCPU: 70, HardIO: 50, MaxWaitSeconds: 1200, Hysteresis: 10}
}

func (c AdmissionPressureConfig) Validate() error {
	if c.Enabled {
		for _, v := range []float64{c.CPUSomeAvg10, c.IOSomeAvg10, c.HardCPU, c.HardIO} {
			if math.IsNaN(v) || math.IsInf(v, 0) || v <= 10 || v > 100 {
				return errors.New("avg10 pressure thresholds must be finite percentages in (10,100]")
			}
		}
		if c.HardCPU < c.CPUSomeAvg10 || c.HardIO < c.IOSomeAvg10 {
			return errors.New("hard pressure limits must be at least the soft thresholds")
		}
		if c.MaxWaitSeconds <= 0 {
			return errors.New("pressure max wait seconds must be positive")
		}
	}
	if c.Mode != "" && c.Mode != PressureObserve && c.Mode != PressureEnforce {
		return errors.New("pressure mode must be observe or enforce")
	}
	for _, value := range []float64{c.IOFullAvg60Max, c.CPUSomeAvg60Max, c.Hysteresis} {
		if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value > 100 {
			return errors.New("pressure thresholds and hysteresis must be finite percentages in [0,100]")
		}
	}
	for _, limit := range []float64{c.IOFullAvg60Max, c.CPUSomeAvg60Max} {
		if limit > 0 && c.Hysteresis >= limit {
			return errors.New("pressure hysteresis must be below each enabled threshold")
		}
	}
	if c.MaxDefer < 0 || ((c.IOFullAvg60Max > 0 || c.CPUSomeAvg60Max > 0) && c.MaxDefer <= 0) {
		return errors.New("enabled pressure admission requires a positive max defer")
	}
	return nil
}

type pressureCheck struct {
	config              AdmissionPressureConfig
	state               model.AdmissionPressure
	sampledAt           time.Time
	lastGrant           time.Time
	ioHeld, cpuHeld     bool
	warned              bool
	deferrals, bypasses uint64
}

func newPressureCheck(config AdmissionPressureConfig) *pressureCheck {
	if config.Mode == "" {
		config.Mode = PressureEnforce
	}
	if !config.Enabled && config.IOFullAvg60Max == 0 && config.CPUSomeAvg60Max == 0 {
		return nil
	}
	if config.Source == nil {
		config.Source = hostPressure
	}
	if config.Logf == nil {
		config.Logf = log.Printf
	}
	if config.Enabled {
		config.Mode = PressureEnforce
		config.Hysteresis = 10
	}
	return &pressureCheck{config: config, state: model.AdmissionPressure{Enabled: true, Mode: config.Mode, CPUSomeAvg10: config.CPUSomeAvg10, IOSomeAvg10: config.IOSomeAvg10, HardCPU: config.HardCPU, HardIO: config.HardIO, MaxWaitSeconds: config.MaxWaitSeconds, IOFullAvg60Max: config.IOFullAvg60Max, CPUSomeAvg60Max: config.CPUSomeAvg60Max, Hysteresis: config.Hysteresis, MaxDeferSeconds: int64(config.MaxDefer / time.Second)}}
}

// Read in admission, never from status/metrics requests. The avg10 gate reads
// before each new grant; legacy avg60 observation caches for one second.
// /proc/pressure reports kernel memory; no source-tree or data-volume IO occurs.
func (p *pressureCheck) refresh(now time.Time) {
	if p == nil || (!p.config.Enabled && !p.sampledAt.IsZero() && now.Sub(p.sampledAt) >= 0 && now.Sub(p.sampledAt) < time.Second) {
		return
	}
	p.sampledAt = now
	wasHeld, wasExceeded, wasAvailable := p.state.Holding, p.state.ThresholdExceeded, p.state.Available
	sample, err := p.config.Source()
	if err == nil {
		err = validatePressureSample(sample)
	}
	p.state.ObservedAt = now
	p.state.Available = err == nil
	if err != nil {
		p.state.Sample = nil
		p.state.Fallback = "PSI unavailable; budget-only admission"
		p.ioHeld, p.cpuHeld = false, false
		if !p.warned {
			p.config.Logf("admission PSI unavailable; pressure check fails open: %v", err)
			p.warned = true
		}
	} else {
		p.warned = false // One warning per outage; recovery re-arms reporting.
		p.state.Fallback = ""
		p.state.Sample = &sample
		if p.config.Enabled {
			p.ioHeld = pressureLatch(p.ioHeld, sample.IO.SomeAvg10, p.config.IOSomeAvg10, 10)
			p.cpuHeld = pressureLatch(p.cpuHeld, sample.CPU.SomeAvg10, p.config.CPUSomeAvg10, 10)
		} else {
			p.ioHeld = pressureLatch(p.ioHeld, sample.IO.FullAvg60, p.config.IOFullAvg60Max, p.config.Hysteresis)
			p.cpuHeld = pressureLatch(p.cpuHeld, sample.CPU.SomeAvg60, p.config.CPUSomeAvg60Max, p.config.Hysteresis)
		}
	}
	p.state.ThresholdExceeded = p.ioHeld || p.cpuHeld
	p.state.Holding = (p.state.ThresholdExceeded || p.settling()) && p.config.Mode == PressureEnforce
	p.state.Resource = ""
	if p.ioHeld {
		p.state.Resource = "io"
	} else if p.cpuHeld {
		p.state.Resource = "cpu"
	}
	if p.ioHeld && p.cpuHeld {
		p.state.Resource = "cpu+io"
	}
	if p.state.Resource == "" && p.settling() {
		p.state.Resource = "settle"
	}
	if wasHeld != p.state.Holding || wasExceeded != p.state.ThresholdExceeded || (!wasAvailable && p.state.Available) {
		p.config.Logf("admission pressure transition: mode=%s available=%t threshold_exceeded=%t holding=%t resource=%s cpu_some_avg10=%.2f io_some_avg10=%.2f", p.state.Mode, p.state.Available, p.state.ThresholdExceeded, p.state.Holding, p.state.Resource, sample.CPU.SomeAvg10, sample.IO.SomeAvg10)
	}
}

// Even a new avg10 read can lag pod startup. Only an idle reading may skip
// settling; starvation grants always require a sample after the full window.
func (p *pressureCheck) settledSample() bool {
	return p.lastGrant.IsZero() || p.sampledAt.Sub(p.lastGrant) >= pressureSettleWindow
}

func (p *pressureCheck) settling() bool {
	return p.config.Enabled && p.state.Available && !p.settledSample() &&
		(p.state.Sample.CPU.SomeAvg10 >= 15 || p.state.Sample.IO.SomeAvg10 >= 10)
}

func pressureLatch(held bool, value, limit, hysteresis float64) bool {
	if limit == 0 {
		return false
	}
	if held {
		return value >= limit-hysteresis
	}
	return value >= limit
}

func (gate *weightedAdmission) pressureWaitLocked(r *admissionReservation) *model.PressureDeferral {
	p := gate.pressure
	if p == nil || !p.state.Holding || !r.pressureDeferred || r.granted {
		return nil
	}
	if p.config.Enabled {
		return &model.PressureDeferral{Resource: p.state.Resource, CPUAvg10: p.state.Sample.CPU.SomeAvg10, IOAvg10: p.state.Sample.IO.SomeAvg10, Avg10Gate: true, Since: r.pressureSince, MaxDeferSeconds: p.config.MaxWaitSeconds}
	}
	value := p.state.Sample.IO.FullAvg60
	if p.state.Resource == "cpu" {
		value = p.state.Sample.CPU.SomeAvg60
	}
	return &model.PressureDeferral{Resource: p.state.Resource, Avg60: value, Since: r.pressureSince, MaxDeferSeconds: int64(p.config.MaxDefer / time.Second)}
}

// Give every prepared, unparked waiter its own clock, even when an earlier
// pressure-held candidate ends the grant pass. Capacity/order still gate grants.
func (gate *weightedAdmission) preparePressureWaitsLocked(now time.Time) {
	p := gate.pressure
	for _, r := range gate.waiters {
		r.pressureDeferred = false
		if p == nil || !p.state.Holding {
			r.pressureSince = time.Time{}
			continue
		}
		if !r.prepared || r.vmParked || gate.kubeVirtBlockedLocked(r) {
			continue
		}
		if holder, ahead := gate.groupBlockerLocked(r); holder != "" || ahead != "" {
			continue
		}
		if p.config.Enabled && gate.pressureBypassLocked(r, now) {
			continue
		}
		if r.pressureSince.IsZero() {
			r.pressureSince = now
			p.deferrals++
			p.config.Logf("admission deferred: %s pressure; run=%s cpu_some_avg10=%.2f io_some_avg10=%.2f", p.state.Resource, r.runID, p.state.Sample.CPU.SomeAvg10, p.state.Sample.IO.SomeAvg10)
		}
		if p.config.Enabled {
			r.pressureDeferred = true
		} else {
			r.pressureDeferred = now.Sub(r.pressureSince) < p.config.MaxDefer
		}
	}
}

func (gate *weightedAdmission) deferPressureLocked(r *admissionReservation, now time.Time) bool {
	p := gate.pressure
	if p == nil || !p.state.Holding {
		return false
	}
	if p.config.Enabled {
		if gate.pressureBypassLocked(r, now) {
			p.bypasses++
			p.config.Logf("admission pressure bypass: run=%s idle_tag=%t cpu_some_avg10=%.2f io_some_avg10=%.2f", r.runID, gate.runs == 0 && !r.pressurePromotion && (r.pressureRelease || r.refKind == model.RefTag), p.state.Sample.CPU.SomeAvg10, p.state.Sample.IO.SomeAvg10)
			return false
		}
		r.pressureDeferred = true
		return true
	}
	if !r.pressureSince.IsZero() && now.Sub(r.pressureSince) >= p.config.MaxDefer {
		p.bypasses++ // Called only immediately before an otherwise valid grant.
		p.config.Logf("admission pressure max defer reached; granting run=%s", r.runID)
		return false
	}
	r.pressureDeferred = true
	return true
}

// Only the first eligible waiter may use the starvation guard. grantLocked
// checks all other gates first; priority alone never authorizes an idle bypass.
func (gate *weightedAdmission) pressureBypassLocked(r *admissionReservation, now time.Time) bool {
	p := gate.pressure
	if p.settling() {
		return false
	}
	if gate.runs == 0 && !r.pressurePromotion && (r.pressureRelease || r.refKind == model.RefTag) {
		return true
	}
	for _, other := range gate.waiters {
		if other == r {
			break
		}
		if other.prepared && !other.vmParked && !gate.kubeVirtBlockedLocked(other) {
			holder, ahead := gate.groupBlockerLocked(other)
			if holder == "" && ahead == "" {
				return false
			}
		}
	}
	since := r.queuedAt
	if since.IsZero() {
		since = r.preparedAt
	}
	return !since.IsZero() && now.Sub(since).Seconds() > float64(p.config.MaxWaitSeconds) && p.state.Sample.CPU.SomeAvg10 < p.config.HardCPU && p.state.Sample.IO.SomeAvg10 < p.config.HardIO
}

func (gate *weightedAdmission) pressureStateLocked() *model.AdmissionPressure {
	if gate.pressure == nil {
		return nil
	}
	result := gate.pressure.state
	if result.Sample != nil {
		sample := *result.Sample
		result.Sample = &sample
	}
	for _, r := range gate.waiters {
		if gate.pressureWaitLocked(r) != nil {
			result.Deferred++
		}
	}
	return &result
}

func validatePressureSample(sample model.PressureSample) error {
	for _, resource := range []model.PressureResource{sample.IO, sample.CPU, sample.Memory} {
		for _, value := range []float64{resource.SomeAvg10, resource.FullAvg10, resource.SomeAvg60, resource.FullAvg60} {
			if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value > 100 {
				return errors.New("invalid PSI percentage")
			}
		}
	}
	return nil
}

func hostPressure() (model.PressureSample, error) {
	if runtime.GOOS != "linux" {
		return model.PressureSample{}, errors.New("host PSI requires Linux")
	}
	return readPressureSources("/proc/pressure", false)
}

func readPressureDirectory(root string) (model.PressureSample, error) {
	return readPressureSources(root, true)
}

// Memory/full rows are optional diagnostics for host admission. Only CPU and
// IO some rows must be available to enforce the avg10 gate.
func readPressureSources(root string, requireDiagnostics bool) (model.PressureSample, error) {
	var sample model.PressureSample
	for name, target := range map[string]*model.PressureResource{"io": &sample.IO, "cpu": &sample.CPU, "memory": &sample.Memory} {
		file, err := os.Open(filepath.Join(root, name)) //nolint:gosec // Fixed procfs names under a trusted source root.
		if err != nil {
			if name == "memory" && !requireDiagnostics {
				continue
			}
			return sample, fmt.Errorf("read PSI %s: %w", name, err)
		}
		data, err := io.ReadAll(io.LimitReader(file, (16<<10)+1))
		_ = file.Close()
		if err != nil {
			if name == "memory" && !requireDiagnostics {
				continue
			}
			return sample, err
		}
		if len(data) > 16<<10 {
			if name == "memory" && !requireDiagnostics {
				continue
			}
			return sample, errors.New("PSI input exceeds limit")
		}
		resource, err := parsePressure(string(data), requireDiagnostics && name != "cpu")
		if err != nil {
			if name == "memory" && !requireDiagnostics {
				continue
			}
			return sample, fmt.Errorf("parse PSI %s: %w", name, err)
		}
		*target = resource
	}
	sample.CPU.FullAvg10, sample.CPU.FullAvg60 = 0, 0 // Undefined at host scope.
	return sample, nil
}

// System CPU full is undefined and may be absent on older Linux kernels.
// Require the some row for all resources and the full row for IO and memory.
func parsePressure(data string, requireFull bool) (model.PressureResource, error) {
	var result model.PressureResource
	seen := map[string]bool{}
	for line := range strings.SplitSeq(data, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		row := fields[0]
		if row != "some" && row != "full" {
			return result, errors.New("unknown PSI row")
		}
		if seen[row] {
			return result, errors.New("duplicate PSI row")
		}
		seen[row] = true
		values := map[string]string{}
		for _, field := range fields[1:] {
			key, value, ok := strings.Cut(field, "=")
			if !ok || values[key] != "" {
				return result, errors.New("invalid PSI field")
			}
			values[key] = value
		}
		for _, key := range []string{"avg10", "avg60", "avg300"} {
			value, err := strconv.ParseFloat(values[key], 64)
			if err != nil || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value > 100 {
				return result, errors.New("invalid PSI average")
			}
			if key == "avg10" {
				if row == "some" {
					result.SomeAvg10 = value
				} else {
					result.FullAvg10 = value
				}
			}
			if key == "avg60" {
				if row == "some" {
					result.SomeAvg60 = value
				} else {
					result.FullAvg60 = value
				}
			}
		}
		if _, err := strconv.ParseUint(values["total"], 10, 64); err != nil {
			return result, errors.New("invalid PSI total")
		}
	}
	if !seen["some"] || (requireFull && !seen["full"]) {
		return result, errors.New("missing PSI row")
	}
	return result, nil
}

// WritePressureMetrics reads only the last admission sample; metrics cannot
// cause procfs reads or influence grant decisions. Labels are fixed resources.
func (scheduler *Scheduler) WritePressureMetrics(w io.Writer) {
	gate := scheduler.admission
	gate.mu.Lock()
	state := gate.pressureStateLocked()
	var deferrals, bypasses uint64
	if p := gate.pressure; p != nil {
		deferrals, bypasses = p.deferrals, p.bypasses
	}
	gate.mu.Unlock()
	for _, metric := range []struct{ name, kind string }{
		{"avg10", "gauge"}, {"avg60", "gauge"}, {"enabled", "gauge"}, {"enforcing", "gauge"}, {"threshold_exceeded", "gauge"}, {"available", "gauge"}, {"holding", "gauge"}, {"deferred", "gauge"}, {"deferrals_total", "counter"}, {"bypasses_total", "counter"},
	} {
		_, _ = fmt.Fprintf(w, "# TYPE oberth_admission_pressure_%s %s\n", metric.name, metric.kind)
	}
	enabled, enforcing, exceeded, available, held, deferred := 0, 0, 0, 0, 0, 0
	if state != nil {
		enabled = 1
		if state.Mode == PressureEnforce {
			enforcing = 1
		}
		if state.ThresholdExceeded {
			exceeded = 1
		}
		if state.Available {
			available = 1
		}
		if state.Holding {
			held = 1
		}
		deferred = state.Deferred
		if sample := state.Sample; sample != nil {
			for _, resource := range []struct {
				name  string
				value model.PressureResource
			}{{"io", sample.IO}, {"cpu", sample.CPU}, {"memory", sample.Memory}} {
				_, _ = fmt.Fprintf(w, "oberth_admission_pressure_avg10{resource=%q,kind=\"some\"} %g\noberth_admission_pressure_avg10{resource=%q,kind=\"full\"} %g\n", resource.name, resource.value.SomeAvg10, resource.name, resource.value.FullAvg10)
				_, _ = fmt.Fprintf(w, "oberth_admission_pressure_avg60{resource=%q,kind=\"some\"} %g\noberth_admission_pressure_avg60{resource=%q,kind=\"full\"} %g\n", resource.name, resource.value.SomeAvg60, resource.name, resource.value.FullAvg60)
			}
		}
	}
	_, _ = fmt.Fprintf(w, "oberth_admission_pressure_enforcing %d\noberth_admission_pressure_threshold_exceeded %d\n", enforcing, exceeded)
	_, _ = fmt.Fprintf(w, "oberth_admission_pressure_enabled %d\noberth_admission_pressure_available %d\noberth_admission_pressure_holding %d\noberth_admission_pressure_deferred %d\noberth_admission_pressure_deferrals_total %d\noberth_admission_pressure_bypasses_total %d\n", enabled, available, held, deferred, deferrals, bypasses)
}
