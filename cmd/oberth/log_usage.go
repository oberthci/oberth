package main

import "github.com/oberthci/oberth/internal/logcontract"

func (s *flowSession) printLogUsage(usage *logcontract.Usage) error {
	if usage == nil {
		return nil
	}
	if err := s.fact("Logs: physical run %s\nRetained: %d body bytes, %d index bytes\nAllocated: %d bytes", usage.PhysicalRunID, usage.Body.Logical, usage.Index.Logical, usage.Body.Allocated+usage.Index.Allocated); err != nil {
		return err
	}
	for _, notice := range []struct {
		show bool
		text string
	}{
		{usage.CaptureLimited, usage.CaptureReason},
		{usage.Incomplete, "capture incomplete"},
		{!usage.AccountingComplete, "accounting incomplete"},
		{usage.PurgePending, "purge pending"},
		{usage.PurgeCompleted, "logs purged"},
		{usage.ProtectionReason != "", usage.ProtectionReason},
	} {
		if notice.show {
			if err := s.fact("%s", notice.text); err != nil {
				return err
			}
		}
	}
	return nil
}
