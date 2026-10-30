package liveness

import (
	"context"
	"time"
)

type observed struct {
	stalled     bool
	errorClass  string
	errorLogged time.Time
}

// transitions is owned by the background observer. Probe requests never log or
// acquire this state, so a blocked log sink cannot block /livez.
type transitions struct {
	previous map[string]observed
	log      func(string, ...any)
}

func (observer *transitions) observe(now time.Time, walk func(func(string, *Heartbeat))) {
	next := make(map[string]observed)
	components := make(map[string]*Heartbeat)
	walk(func(component string, heartbeat *Heartbeat) { components[component] = heartbeat })
	writerClass := "none"
	if writer := components["writer"]; writer != nil {
		writerClass = writer.ErrorClass()
	}
	for component, heartbeat := range components {
		before := observer.previous[component]
		class := heartbeat.ErrorClass()
		stalled := heartbeat.CheckAt(now) != nil
		age := time.Duration(0)
		if sample := heartbeat.state.Load(); sample != nil && !sample.suspended {
			age = max(time.Duration(0), now.Sub(sample.progressed))
		}
		if stalled != before.stalled {
			state := "live"
			if stalled {
				state = "stalled"
			}
			observer.log("liveness: component=%q state=%s age=%s last_writer_error=%s", component, state, age, writerClass)
		}
		errorLogged := before.errorLogged
		if class != "none" && (class != before.errorClass || now.Sub(errorLogged) >= time.Minute) {
			observer.log("liveness: component=%q event=probe_error error_class=%s age=%s", component, class, age)
			errorLogged = now
		} else if class == "none" && before.errorClass != "" && before.errorClass != "none" {
			observer.log("liveness: component=%q event=probe_recovered previous_error_class=%s", component, before.errorClass)
		}
		next[component] = observed{stalled: stalled, errorClass: class, errorLogged: errorLogged}
	}
	for component, before := range observer.previous {
		if _, found := next[component]; !found && before.stalled {
			observer.log("liveness: component=%q state=live reason=worker_finished", component)
		}
	}
	observer.previous = next // finished promotion IDs do not accumulate
}

// WatchTransitions logs each local component's live/stalled transitions, plus
// error-class changes and at most one repeated probe error per minute.
func WatchTransitions(ctx context.Context, interval time.Duration, walk func(func(string, *Heartbeat)), log func(string, ...any)) {
	observer := transitions{log: log}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for ctx.Err() == nil {
		observer.observe(time.Now(), walk)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
