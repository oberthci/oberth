package store

// Changes is shared by service and native writers using this store instance.
// A completed transaction closes the observed channel and replaces it atomically.
func (s *Store) Changes() <-chan struct{} {
	s.changeMu.Lock()
	defer s.changeMu.Unlock()
	return s.changed
}

func (s *Store) signalChange() {
	started := s.options.Clock()
	defer func() { s.metrics.fanout.record(s.options.Clock().Sub(started)) }()
	s.changeMu.Lock()
	defer s.changeMu.Unlock()
	close(s.changed)
	s.changed = make(chan struct{})
}
