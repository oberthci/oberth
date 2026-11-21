package service_test

import (
	"fmt"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/publicerr"
	"strings"
	"testing"
)

func TestIssueImportHeaderCountRefusesTruncatedAndExtraRecords(t *testing.T) {
	for _, real := range []bool{false, true} {
		t.Run(fmt.Sprint(real), func(t *testing.T) {
			p := people(t)
			if !real {
				p.f.c.People.Issues = p.f.store.native
				p.f.restart(t)
			}
			for _, data := range []string{
				"{\"count\":2}\n" + neutralLine(7),
				"{\"count\":1}\n" + neutralLine(7) + "\n" + neutralLine(8),
				"{\"count\":0}\n" + neutralLine(7),
				neutralLine(7),
			} {
				_, err := importCall(t, p, apicontract.IssueImportArgs{JSONL: data})
				if err == nil || !strings.Contains(strings.Join(publicerr.Classify(err).Context, " "), "line 1") {
					t.Fatal("count or header accepted", err)
				}
				if _, err := p.f.s.PrepareIssueGet(t.Context(), adminCaller(), apicontract.IssueGetArgs{ID: 7}); err == nil {
					t.Fatal("incomplete file wrote an issue")
				}
			}
		})
	}
}
