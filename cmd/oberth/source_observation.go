package main

import (
	"fmt"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/oberthci/oberth/internal/sourcedelivery"
)

func sourceOutcomes() [10]string {
	return [...]string{"ok", "unavailable", "forbidden", "unauthenticated", "invalid_request", "too_large", "gone", "not_configured", "delivery_failed", "cancelled"}
}
func sourceCauses() [8]string {
	return [...]string{"none", "other", "forge-timeout", "forge-refused", "contention", "auth-cap", "transfer-slot", "export-slot"}
}
func sourceKinds() [3]string { return [...]string{"load", "receive", "job"} }
func sourceDurationBounds() [7]time.Duration {
	return [...]time.Duration{time.Millisecond, 10 * time.Millisecond, 100 * time.Millisecond, time.Second, 10 * time.Second, time.Minute, 10 * time.Minute}
}

type sourceCounters struct {
	causes                                 [8]atomic.Uint64
	duration                               [3][8]atomic.Uint64
	total                                  [3]atomic.Uint64
	permits                                sourcedelivery.Permits
	outcomes                               [3][10]atomic.Uint64
	waits, service                         [sourcedelivery.PhaseCount]atomic.Uint64
	blocked                                [sourcedelivery.PhaseCount]atomic.Uint64
	bytes, cancellations, leaders, joiners atomic.Uint64
}

func (o *serverOwners) sourceObservation(s sourcedelivery.Summary) {
	c := &o.c.health.source
	kind, code := 2, 1
	for i, name := range sourceKinds() {
		if s.Kind == name {
			kind = i
		}
	}
	for i, name := range sourceOutcomes() {
		if s.Code == name {
			code = i
		}
	}
	c.outcomes[kind][code].Add(1)
	c.recordCause(s)
	c.recordDuration(kind, s.Total)
	for p := range sourcedelivery.PhaseCount {
		c.waits[p].Add(uint64(max(0, s.Wait[p])))
		c.service[p].Add(uint64(max(0, s.Service[p])))
	}
	if s.Blocked && s.Phase < sourcedelivery.PhaseCount {
		c.blocked[s.Phase].Add(1)
	}
	if s.Cancelled {
		c.cancellations.Add(1)
	}
	c.bytes.Add(uint64(max(0, s.Bytes)))
	c.leaders.Add(s.Leaders)
	c.joiners.Add(s.Joiners)
	reason := "Source observation"
	detail := s.Detail()
	if capacitySummary(s) {
		reason = sourcedelivery.CapacityReason
		detail = sourcedelivery.CapacityRefusal + " " + sourcedelivery.CapacityRemedy + " " + detail
	}
	o.cacheObservation(reason, detail)
}

func capacitySummary(s sourcedelivery.Summary) bool {
	if !s.Blocked || s.Code != "unavailable" {
		return false
	}
	if s.Cancelled {
		return true
	}
	switch s.Reason {
	case "auth-cap", "export-slot", "transfer-slot":
		return true
	}
	return false
}

func (c *sourceCounters) recordDuration(kind int, total time.Duration) {
	c.total[kind].Add(uint64(max(0, total)))
	bounds := sourceDurationBounds()
	bucket := len(bounds)
	for i, bound := range bounds {
		if total <= bound {
			bucket = i
			break
		}
	}
	c.duration[kind][bucket].Add(1)
}

func (c *sourceCounters) recordCause(s sourcedelivery.Summary) {
	cause := 1
	if s.Code == "ok" {
		cause = 0
	}
	for i, name := range sourceCauses() {
		if s.Reason == name {
			cause = i
		}
	}
	c.causes[cause].Add(1)
}

func (c *sourceCounters) metrics() string {
	var out strings.Builder
	bounds := sourceDurationBounds()
	for k, kind := range sourceKinds() {
		var count uint64
		for i := range c.duration[k] {
			count += c.duration[k][i].Load()
			bound := "+Inf"
			if i < len(bounds) {
				bound = fmt.Sprint(bounds[i].Seconds())
			}
			_, _ = fmt.Fprintf(&out, "oberth_source_duration_seconds_bucket{kind=%q,le=%q} %d\n", kind, bound, count)
		}
		_, _ = fmt.Fprintf(&out, "oberth_source_duration_seconds_sum{kind=%q} %g\noberth_source_duration_seconds_count{kind=%q} %d\n", kind, float64(c.total[k].Load())/float64(time.Second), kind, count)
		for i, code := range sourceOutcomes() {
			_, _ = fmt.Fprintf(&out, "oberth_source_outcomes_total{kind=%q,code=%q} %d\n", kind, code, c.outcomes[k][i].Load())
		}
	}
	for i, cause := range sourceCauses() {
		_, _ = fmt.Fprintf(&out, "oberth_source_causes_total{reason=%q} %d\n", cause, c.causes[i].Load())
	}
	for p := range sourcedelivery.PhaseCount {
		_, _ = fmt.Fprintf(&out, "oberth_source_active{phase=%q} %d\noberth_source_peak{phase=%q} %d\n", p.String(), c.permits.Active[p].Load(), p.String(), c.permits.Peak[p].Load())
		_, _ = fmt.Fprintf(&out, "oberth_source_wait_nanoseconds_total{phase=%q} %d\noberth_source_service_nanoseconds_total{phase=%q} %d\noberth_source_blocked_total{phase=%q} %d\n", p.String(), c.waits[p].Load(), p.String(), c.service[p].Load(), p.String(), c.blocked[p].Load())
	}
	_, _ = fmt.Fprintf(&out, "oberth_source_bytes_total %d\noberth_source_cancellations_total %d\noberth_source_leaders_total %d\noberth_source_joiners_total %d\n", c.bytes.Load(), c.cancellations.Load(), c.leaders.Load(), c.joiners.Load())
	return out.String()
}

// Detail is lossy operator information. Its cap includes the truncation flag
// and never splits a UTF-8 rune, even with a stalled maintenance log consumer.
func boundedObservation(detail string) string {
	const limit = 2048
	const flag = " truncated=true"
	detail = strings.ToValidUTF8(detail, "?")
	if len(detail) <= limit {
		return detail
	}
	n := limit - len(flag)
	for n > 0 && !utf8.RuneStart(detail[n]) {
		n--
	}
	return detail[:n] + flag
}
