package store

import "github.com/oberthci/oberth/internal/stateport"

func (s *Store) Subscribe(scope stateport.ChangeScope) stateport.Subscription {
	return s.notifications.Subscribe(scope)
}

// noteChange is held by the writer slot. Rollback discards these hints; only
// publishChanges transfers them to observers after commit.
func (s *Store) noteChange(change stateport.Change) {
	if s.pendingChanges == nil {
		s.pendingChanges = make(map[stateport.Change]struct{})
	}
	if len(s.pendingChanges) >= 256 {
		// A wide atomic batch uses one explicit broadcast hint instead of
		// growing writer memory or losing the need to recheck durable state.
		clear(s.pendingChanges)
		s.pendingChanges[stateport.Change{Broadcast: true}] = struct{}{}
	}
	if _, wide := s.pendingChanges[stateport.Change{Broadcast: true}]; wide {
		return
	}
	s.pendingChanges[change] = struct{}{}
}

func (s *Store) publishChanges() {
	changes := make([]stateport.Change, 0, len(s.pendingChanges))
	for c := range s.pendingChanges {
		changes = append(changes, c)
	}
	s.pendingChanges = nil
	if len(changes) > 0 {
		started := s.options.Clock()
		s.notifications.Publish(changes)
		s.metrics.fanout.record(s.options.Clock().Sub(started))
	}
}
