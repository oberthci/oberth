package service_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/service"
)

func TestIssueImportDiagnosticsNameLineAndKnownNumber(t *testing.T) {
	for position := range 3 {
		for key, change := range map[string]func(string) string{
			"title":   func(s string) string { return strings.Replace(s, `"title":"task"`, `"title":" "`, 1) },
			"history": func(s string) string { return strings.Replace(s, `"history":[]`, `"history":[{"kind":"bad"}]`, 1) },
			"unknown": func(s string) string { return strings.Replace(s, `"id":2`, `"id":2,"unknown":true`, 1) },
			"line":    func(s string) string { return s + strings.Repeat(" ", apicontract.AdminBytes) },
		} {
			t.Run(fmt.Sprint(position, key), func(t *testing.T) {
				lines := []string{neutralLine(1), neutralLine(2), neutralLine(3)}
				lines[position] = change(neutralLine(2))
				_, err := service.DecodeIssueImport(apicontract.IssueImportArgs{JSONL: countedJSONL(strings.Join(lines, "\n"))})
				if err == nil {
					t.Fatal("accepted malformed file")
				}
				p := publicerr.Classify(err)
				context := strings.Join(p.Context, " ")
				if !strings.Contains(context, fmt.Sprintf("line %d", position+2)) || !strings.Contains(context, "No issues imported.") {
					t.Fatal(context)
				}
				if (key == "title" || key == "history") && !strings.Contains(context, "issue 2") {
					t.Fatal(context)
				}
			})
		}
	}
}

func TestIssueImportSizeAndProjectionHaveDistinctReasons(t *testing.T) {
	line := neutralLine(1)
	projection := strings.Replace(line, `"body":"body"`, `"body":"`+strings.Repeat("b", apicontract.AdminBytes-len(line)-100)+`"`, 1)
	reasons := map[string]bool{}
	for _, data := range []string{strings.Repeat(" ", apicontract.RequestBytes+1), strings.Repeat(" ", apicontract.AdminBytes+1), countedJSONL(projection)} {
		_, err := importCall(t, people(t), apicontract.IssueImportArgs{JSONL: data, DryRun: true})
		if err == nil {
			t.Fatal("accepted invalid file")
		}
		reason := publicerr.Classify(err).Reason()
		if reasons[reason] {
			t.Fatal("different refusals share a reason", reason)
		}
		reasons[reason] = true
	}
}

func TestIssueImportConflictHasLocationAndPreservesEarlierFile(t *testing.T) {
	for _, real := range []bool{false, true} {
		t.Run(fmt.Sprint(real), func(t *testing.T) {
			p := people(t)
			if !real {
				p.f.c.People.Issues = p.f.store.native
				p.f.restart(t)
			}
			first := importRecord(9)
			if _, err := importCall(t, p, importArgs(t, first)); err != nil {
				t.Fatal(err)
			}
			_, err := importCall(t, p, importArgs(t, importRecord(7), first))
			if err == nil {
				t.Fatal("accepted overlap")
			}
			raw, _ := json.Marshal(publicerr.Classify(err))
			if !strings.Contains(string(raw), "line 3, issue 9") || !strings.Contains(string(raw), "No issues imported.") || strings.Contains(string(raw), "current identity") {
				t.Fatal(string(raw))
			}
			checkImportedRead(t, p, first)
		})
	}
}
