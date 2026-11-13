package service

func (reservation *submissionOwner) markSubmitted() {
	reservation.gate.mu.Lock()
	defer reservation.gate.mu.Unlock()
	reservation.submitted = true
}

// Terminal observation releases retained submission ownership. Stop acceptance
// alone cannot release a Workflow while its exit handlers still run.
func (gate *submissionRegistry) releaseTerminalRun(runID string) {
	gate.mu.Lock()
	defer gate.mu.Unlock()
	for _, reservation := range gate.owners {
		if reservation.runID == runID && reservation.granted && reservation.submitted {
			reservation.retained = false
			gate.releaseLocked(reservation)
			return
		}
	}
}
