package service_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/service"
)

func neutralLine(id int) string {
	return fmt.Sprintf(`{"id":%d,"upstream":"origin","title":"task","body":"body","state":"open","created_at":"2020-01-01T00:00:00+02:00","closed_at":null,"author":"text","closer":"","history":[]}`, id)
}
func TestIssueImportStrictJSONAndValidation(t *testing.T) {
	base := neutralLine(1)
	cases := map[string]string{
		"unknown":        strings.Replace(base, `"id":1`, `"id":1,"unknown":true`, 1),
		"nested unknown": strings.Replace(base, `"history":[]`, `"history":[{"kind":"comment","at":"2020-01-01T00:00:00Z","author":"a","unknown":true}]`, 1),
		"duplicate ID":   base + "\n" + base,
		"empty":          "", "blank line": base + "\n\n", "null": "null", "array": "[]", "trailing": base + "{}",
		"zero ID":              strings.Replace(base, `"id":1`, `"id":0`, 1),
		"fractional ID":        strings.Replace(base, `"id":1`, `"id":1.5`, 1),
		"exhausted ID":         strings.Replace(base, `"id":1`, `"id":9223372036854775807`, 1),
		"invalid state":        strings.Replace(base, `"state":"open"`, `"state":"invalid"`, 1),
		"missing close time":   strings.Replace(base, `"state":"open"`, `"state":"closed"`, 1),
		"null creation":        strings.Replace(base, `"2020-01-01T00:00:00+02:00"`, `null`, 1),
		"invalid history kind": strings.Replace(base, `"history":[]`, `"history":[{"kind":"grant","at":"2020-01-01T00:00:00Z","author":"a"}]`, 1),
		"invalid history time": strings.Replace(base, `"history":[]`, `"history":[{"kind":"comment","at":null,"author":"a"}]`, 1),
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := service.DecodeIssueImport(apicontract.IssueImportArgs{JSONL: countedJSONL(data)}); err == nil {
				t.Fatal("invalid import accepted")
			}
		})
	}
	q, err := service.DecodeIssueImport(apicontract.IssueImportArgs{JSONL: countedJSONL(base + "\n")})
	if err != nil || q.Records[0].Kind != "manual" || q.Records[0].WorkStatus != "todo" || q.Records[0].Occurrences != 1 {
		t.Fatal("defaults", q, err)
	}
}

func TestIssueImportExactLineAndFileBounds(t *testing.T) {
	for _, extra := range []int{0, 1} {
		line := neutralLine(1)
		line += strings.Repeat(" ", apicontract.AdminBytes-len(line)+extra)
		_, err := service.DecodeIssueImport(apicontract.IssueImportArgs{JSONL: countedJSONL(line)})
		if (err != nil) != (extra == 1) {
			t.Fatal("line bound", extra, err)
		}
		lines := make([]string, 9)
		header := `{"count":9}` + "\n"
		remaining := apicontract.RequestBytes + extra - len(header)
		for i := range lines {
			n := remaining / (len(lines) - i)
			lines[i] = neutralLine(i + 1)
			lines[i] += strings.Repeat(" ", n-len(lines[i])-1)
			remaining -= n
		}
		data := header + strings.Join(lines, "\n") + "\n"
		if len(data) != apicontract.RequestBytes+extra {
			t.Fatal("fixture bound")
		}
		_, err = service.DecodeIssueImport(apicontract.IssueImportArgs{JSONL: data})
		if (err != nil) != (extra == 1) {
			t.Fatal("file bound", extra, err)
		}
	}
}

func TestIssueImportRefusesUnreadableProjection(t *testing.T) {
	// The line fits, but provenance and the ordinary issue envelope must fit too.
	line := neutralLine(1)
	line = strings.Replace(line, `"body":"body"`, `"body":"`+strings.Repeat("b", apicontract.AdminBytes-len(line)-100)+`"`, 1)
	if len(line) >= apicontract.AdminBytes {
		t.Fatal("fixture line")
	}
	if _, err := importCall(t, people(t), apicontract.IssueImportArgs{JSONL: countedJSONL(line), DryRun: true}); err == nil {
		t.Fatal("unreadable issue accepted")
	}
}

func TestIssueImportPreservesArrayOrderAndTimestampOffsets(t *testing.T) {
	line := strings.Replace(neutralLine(1), `"history":[]`, `"history":[{"kind":"comment","at":"2022-01-01T00:00:00.123456789+02:00","author":"second","text":"one"},{"kind":"comment","at":"2021-01-01T00:00:00Z","author":"first","text":"two"}]`, 1)
	q, err := service.DecodeIssueImport(apicontract.IssueImportArgs{JSONL: countedJSONL(line)})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(q.Records[0])
	if err != nil || !strings.Contains(string(raw), `2022-01-01T00:00:00.123456789+02:00`) || q.Records[0].History[0].Text != "one" {
		t.Fatal("time or order changed", err, string(raw))
	}
}
