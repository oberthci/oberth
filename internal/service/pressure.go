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

// AdmissionPressureConfig gates only new grants. Zero thresholds disable it.
// Source and Logf permit deterministic tests without observing the test host.
type AdmissionPressureConfig struct {
	IOFullAvg60Max  float64
	CPUSomeAvg60Max float64
	Hysteresis      float64
	MaxDefer        time.Duration
	Source          func() (model.PressureSample, error)
	Logf            func(string, ...any)
}

func (c AdmissionPressureConfig) Validate() error {
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
	ioHeld, cpuHeld     bool
	warned              bool
	deferrals, bypasses uint64
}

func newPressureCheck(config AdmissionPressureConfig) *pressureCheck {
	if config.IOFullAvg60Max == 0 && config.CPUSomeAvg60Max == 0 {
		return nil
	}
	if config.Source == nil {
		config.Source = hostPressure
	}
	if config.Logf == nil {
		config.Logf = log.Printf
	}
	return &pressureCheck{config: config, state: model.AdmissionPressure{Enabled: true, IOFullAvg60Max: config.IOFullAvg60Max, CPUSomeAvg60Max: config.CPUSomeAvg60Max, Hysteresis: config.Hysteresis, MaxDeferSeconds: int64(config.MaxDefer / time.Second)}}
}

// Read at most once per second in admission, never from status/metrics requests.
// /proc/pressure reports kernel memory; no source-tree or data-volume IO occurs.
func (p *pressureCheck) refresh(now time.Time) {
	if p == nil || (!p.sampledAt.IsZero() && now.Sub(p.sampledAt) >= 0 && now.Sub(p.sampledAt) < time.Second) {
		return
	}
	p.sampledAt = now
	wasHeld, wasAvailable := p.state.Holding, p.state.Available
	sample, err := p.config.Source()
	if err == nil {
		err = validatePressureSample(sample)
	}
	p.state.ObservedAt = now
	p.state.Available = err == nil
	if err != nil {
		p.state.Sample = nil
		p.ioHeld, p.cpuHeld = false, false
		if !p.warned {
			p.config.Logf("admission PSI unavailable; pressure check fails open: %v", err)
			p.warned = true
		}
	} else {
		p.warned = false // One warning per outage; recovery re-arms reporting.
		p.state.Sample = &sample
		p.ioHeld = pressureLatch(p.ioHeld, sample.IO.FullAvg60, p.config.IOFullAvg60Max, p.config.Hysteresis)
		p.cpuHeld = pressureLatch(p.cpuHeld, sample.CPU.SomeAvg60, p.config.CPUSomeAvg60Max, p.config.Hysteresis)
	}
	p.state.Holding = p.ioHeld || p.cpuHeld
	p.state.Resource = ""
	if p.ioHeld {
		p.state.Resource = "io"
	} else if p.cpuHeld {
		p.state.Resource = "cpu"
	}
	if wasHeld != p.state.Holding || (!wasAvailable && p.state.Available) {
		p.config.Logf("admission pressure transition: available=%t holding=%t resource=%s", p.state.Available, p.state.Holding, p.state.Resource)
	}
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
		if r.pressureSince.IsZero() {
			r.pressureSince = now
			p.deferrals++
			p.config.Logf("admission deferred: %s pressure; run=%s", p.state.Resource, r.runID)
		}
		r.pressureDeferred = now.Sub(r.pressureSince) < p.config.MaxDefer
	}
}

func (gate *weightedAdmission) deferPressureLocked(r *admissionReservation, now time.Time) bool {
	p := gate.pressure
	if p == nil || !p.state.Holding {
		return false
	}
	if !r.pressureSince.IsZero() && now.Sub(r.pressureSince) >= p.config.MaxDefer {
		p.bypasses++ // Called only immediately before an otherwise valid grant.
		p.config.Logf("admission pressure max defer reached; granting run=%s", r.runID)
		return false
	}
	r.pressureDeferred = true
	return true
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
		for _, value := range []float64{resource.SomeAvg60, resource.FullAvg60} {
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
	return readPressureDirectory("/proc/pressure")
}

func readPressureDirectory(root string) (model.PressureSample, error) {
	var sample model.PressureSample
	for name, target := range map[string]*model.PressureResource{"io": &sample.IO, "cpu": &sample.CPU, "memory": &sample.Memory} {
		file, err := os.Open(filepath.Join(root, name)) //nolint:gosec // Fixed procfs names under a trusted source root.
		if err != nil {
			return sample, fmt.Errorf("read PSI %s: %w", name, err)
		}
		data, err := io.ReadAll(io.LimitReader(file, (16<<10)+1))
		_ = file.Close()
		if err != nil {
			return sample, err
		}
		if len(data) > 16<<10 {
			return sample, errors.New("PSI input exceeds limit")
		}
		*target, err = parsePressure(string(data), name != "cpu")
		if err != nil {
			return sample, fmt.Errorf("parse PSI %s: %w", name, err)
		}
	}
	sample.CPU.FullAvg60 = 0 // Undefined at host scope.
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
		{"avg60", "gauge"}, {"enabled", "gauge"}, {"available", "gauge"}, {"holding", "gauge"}, {"deferred", "gauge"}, {"deferrals_total", "counter"}, {"bypasses_total", "counter"},
	} {
		_, _ = fmt.Fprintf(w, "# TYPE oberth_admission_pressure_%s %s\n", metric.name, metric.kind)
	}
	enabled, available, held, deferred := 0, 0, 0, 0
	if state != nil {
		enabled = 1
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
				_, _ = fmt.Fprintf(w, "oberth_admission_pressure_avg60{resource=%q,kind=\"some\"} %g\noberth_admission_pressure_avg60{resource=%q,kind=\"full\"} %g\n", resource.name, resource.value.SomeAvg60, resource.name, resource.value.FullAvg60)
			}
		}
	}
	_, _ = fmt.Fprintf(w, "oberth_admission_pressure_enabled %d\noberth_admission_pressure_available %d\noberth_admission_pressure_holding %d\noberth_admission_pressure_deferred %d\noberth_admission_pressure_deferrals_total %d\noberth_admission_pressure_bypasses_total %d\n", enabled, available, held, deferred, deferrals, bypasses)
}
