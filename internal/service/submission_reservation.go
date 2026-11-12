package service

func (reservation *submissionOwner) markSubmitted() {
	reservation.gate.mu.Lock()
	defer reservation.gate.mu.Unlock()
	reservation.submitted = true
}

// Accepted deletion releases a submitted Workflow synchronously, without waiting
// for its observer. During creation, Delete may only record cancellation intent;
// the creating background task must finish cleanup before its reservation can be released.
func (gate *submissionRegistry) releaseDeletedRun(runID string) {
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
