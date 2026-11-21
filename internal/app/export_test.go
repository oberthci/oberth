package app

import (
	"context"
	"github.com/oberthci/oberth/internal/admissioncontract"
)

// ReceiveStashSize observes only C3's process-local ownership in package tests.
func ReceiveStashSize(s *Submitter) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.receives)
}

// Admit exposes preparation only to contract fixtures; production enters through the gate or starter.
func (s *Submitter) Admit(ctx context.Context, r Request, source Source) (*admissioncontract.PreparedAdmission, error) {
	run, err := s.admit(ctx, r, source, admitOptions{mode: admissioncontract.ModeSubmit})
	return run.Prepared, err
}
