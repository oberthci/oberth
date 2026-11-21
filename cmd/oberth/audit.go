package main

import (
	"encoding/json"
	"github.com/oberthci/oberth/internal/apicontract"
	"slices"
	"strconv"
)

func audit(s *flowSession, args []string) error {
	f := flowFlags("audit", s.app.home())
	rest, err := parseFlow(f, args)
	if err != nil {
		return err
	}
	if !validFlowArgs("audit", rest) {
		return usageProblems{"audit accepts flags only"}
	}
	var high int64
	for {
		next, err := s.drainAudit(high)
		if err != nil {
			return err
		}
		if next > high {
			high = next
			continue
		}
		return nil
	}
}
func (s *flowSession) drainAudit(high int64) (int64, error) {
	var before int64
	newHigh := high
	var collected []apicontract.AuditRecord
	for {
		var page apicontract.AuditPage
		if err := s.invoke("audit_list", apicontract.AuditListArgs{Before: before, Limit: 200}, &page); err != nil {
			return high, err
		}
		additions, stop := auditAfter(page.Records, high)
		collected = append(collected, additions...)
		for _, record := range additions {
			newHigh = max(newHigh, record.ID)
		}
		if stop || page.NextCursor == "" {
			break
		}
		next, err := strconv.ParseInt(page.NextCursor, 10, 64)
		if err != nil || next <= 0 || before > 0 && next >= before {
			return high, flowError("cursor", err)
		}
		before = next
	}
	if err := s.emitAudit(collected); err != nil {
		return high, err
	}
	s.last = nil
	return newHigh, nil
}

func (s *flowSession) emitAudit(collected []apicontract.AuditRecord) error {
	slices.SortFunc(collected, func(a, b apicontract.AuditRecord) int {
		if a.ID < b.ID {
			return -1
		}
		if a.ID > b.ID {
			return 1
		}
		return 0
	})
	var emitted int64
	for _, record := range collected {
		if record.ID == emitted {
			continue
		}
		emitted = record.ID
		data, _ := json.Marshal(record)
		if s.options.json {
			if _, err := s.app.out.Write(append(data, '\n')); err != nil {
				return err
			}
		} else {
			if err := s.fact("%d %s %s", record.ID, record.Action, record.ResourceID); err != nil {
				return err
			}
		}
	}
	return nil
}

func auditAfter(records []apicontract.AuditRecord, high int64) ([]apicontract.AuditRecord, bool) {
	var kept []apicontract.AuditRecord
	stop := false
	for _, r := range records {
		if r.ID <= high {
			stop = true
		} else {
			kept = append(kept, r)
		}
	}
	return kept, stop
}
