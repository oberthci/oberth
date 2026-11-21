package store

// Stage is owned, locked private scratch space for recovery. Close removes it;
// Open and the next NewStage sweep inactive owned remnants after process death.
type Stage struct {
	Path    string
	cleanup func() error
}

func NewStage(parent string) (*Stage, error) {
	path, cleanup, err := newStage(parent)
	if err != nil {
		return nil, err
	}
	return &Stage{Path: path, cleanup: cleanup}, nil
}

func (s *Stage) Close() error {
	if s == nil || s.cleanup == nil {
		return nil
	}
	err := s.cleanup()
	s.cleanup = nil
	return err
}
