package service_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/publicerr"
)

func TestIssueImportAcceptsManualOnlyBeforeWriting(t *testing.T) {
	for _, real := range []bool{false, true} {
		t.Run(fmt.Sprint(real), func(t *testing.T) {
			p := people(t)
			if !real {
				p.f.c.People.Issues = p.f.store.native
				p.f.restart(t)
			}
			for _, kind := range []model.IssueKind{model.IssueCI, "future"} {
				v := importRecord(42)
				v.Kind, v.Repo, v.Branch = kind, "origin/team/repo", "refs/heads/main"
				_, err := importCall(t, p, importArgs(t, importRecord(7), v))
				if err == nil || !strings.Contains(strings.Join(publicerr.Classify(err).Context, " "), "line 3, issue 42") {
					t.Fatal("nonmanual import did not name its location", err)
				}
				if _, err := p.f.s.PrepareIssueGet(t.Context(), adminCaller(), apicontract.IssueGetArgs{ID: 7}); err == nil {
					t.Fatal("nonmanual record allowed a partial import")
				}
			}
		})
	}
}
