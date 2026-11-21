package argojob

// Pending dispatch belongs to tracking: untracking drops it without leaving
// completed-run jobs in a second queue. A newer step snapshot replaces its slot.
func (e *Executor) offerCapture(t *tracking, job captureJob) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.tracked[t.binding.Workflow.Name] != t {
		return
	}
	entries := job.entries[:0]
	for _, entry := range job.entries {
		key := entry.PodUID + "/" + entry.Container
		if !t.captures[key] {
			entries = append(entries, entry)
		}
	}
	if len(entries) == 0 {
		return
	}
	if t.pending == nil {
		t.pending = map[string]captureJob{}
	}
	t.pending[entries[0].Step] = captureJob{binding: job.binding, entries: entries}
	e.signalCapture()
}
func (e *Executor) nextCapture() (captureJob, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for name, t := range e.tracked {
		for step, job := range t.pending {
			if e.expired(t) {
				clear(t.pending)
				e.queue.Add(name)
				break
			}
			job.until = t.evidenceUntil
			delete(t.pending, step)
			for _, entry := range job.entries {
				t.captures[entry.PodUID+"/"+entry.Container] = true
			}
			e.queue.Add(name)
			e.signalCapture()
			return job, true
		}
	}
	return captureJob{}, false
}
func (e *Executor) signalCapture() {
	select {
	case e.captureReady <- struct{}{}:
	default:
	}
}
