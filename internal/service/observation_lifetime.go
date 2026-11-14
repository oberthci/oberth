package service

func (scheduler *Scheduler) closeObservations() {
	scheduler.observationMu.Lock()
	scheduler.observationsClosed = true
	scheduler.stopObservations()
	scheduler.observationMu.Unlock()
	scheduler.observers.Wait()
}
