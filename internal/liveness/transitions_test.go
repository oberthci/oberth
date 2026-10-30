package liveness

import (
	"context"
	"fmt"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestLivenessTransitionsLogOnceWithComponentAgeAndErrorClass(t *testing.T) {
	now := time.Now()
	h := &Heartbeat{Now: func() time.Time { return now }}
	h.Beat()
	var logs []string
	observer := transitions{log: func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }}
	walk := func(visit func(string, *Heartbeat)) { visit("writer", h) }
	observer.observe(now, walk)
	if len(logs) != 0 {
		t.Fatal("initial healthy sample logged")
	}
	now = now.Add(MaxStall + time.Second)
	h.probe.Store(&probeResult{err: context.DeadlineExceeded, class: "deadline"})
	observer.observe(now, walk)
	observer.observe(now, walk)
	if len(logs) != 2 || !strings.Contains(logs[0], `component="writer" state=stalled age=5m1s last_writer_error=deadline`) {
		t.Fatalf("missing or repeated stall/error transition: %v", logs)
	}
	h.probe.Store(&probeResult{class: "none"})
	h.Beat()
	observer.observe(now, walk)
	observer.observe(now, walk)
	if len(logs) != 4 || !strings.Contains(logs[2], "state=live") || !strings.Contains(logs[3], "event=probe_recovered") {
		t.Fatalf("missing or repeated recovery: %v", logs)
	}
}

func TestLivenessProbeErrorsAreRateLimitedAndFinishedWorkersAreForgotten(t *testing.T) {
	now := time.Now()
	var h Heartbeat
	h.Beat()
	h.probe.Store(&probeResult{err: syscall.ENOSPC, class: "disk_full"})
	var logs []string
	observer := transitions{log: func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }}
	walk := func(visit func(string, *Heartbeat)) { visit("writer", &h) }
	observer.observe(now, walk)
	observer.observe(now.Add(30*time.Second), walk)
	observer.observe(now.Add(time.Minute), walk)
	if len(logs) != 2 {
		t.Fatalf("persistent hard errors not rate limited: %v", logs)
	}
	h.probe.Store(&probeResult{err: syscall.EROFS, class: "read_only"})
	observer.observe(now.Add(time.Minute+time.Second), walk)
	if len(logs) != 3 || !strings.Contains(logs[2], "error_class=read_only") {
		t.Fatal("changed error class was hidden", logs)
	}
	promotion := func(visit func(string, *Heartbeat)) { visit("promotion queued-id", &h) }
	observer.observe(now.Add(24*time.Hour), promotion)
	logs = nil
	observer.observe(now.Add(24*time.Hour), func(func(string, *Heartbeat)) {})
	if len(logs) != 1 || !strings.Contains(logs[0], `component="promotion queued-id" state=live reason=worker_finished`) || len(observer.previous) != 0 {
		t.Fatal("finished promotion state leaked", logs)
	}
}

func TestLivenessWorkerTransitionIncludesLastWriterErrorClass(t *testing.T) {
	now := time.Now()
	writer := &Heartbeat{Now: func() time.Time { return now }}
	scheduler := &Heartbeat{Now: func() time.Time { return now }}
	writer.Beat()
	scheduler.Beat()
	now = now.Add(MaxStall + time.Second)
	writer.Beat()
	writer.probe.Store(&probeResult{err: sqliteProbeError(5), class: "busy"})
	var logs []string
	observer := transitions{log: func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }}
	observer.observe(now, func(visit func(string, *Heartbeat)) { visit("scheduler", scheduler); visit("writer", writer) })
	for _, entry := range logs {
		if strings.Contains(entry, `component="scheduler" state=stalled`) && strings.Contains(entry, "last_writer_error=busy") {
			return
		}
	}
	t.Fatal("worker stall omitted the writer error class", logs)
}
