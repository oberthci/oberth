package service_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/model"
)

func TestIssueImportDisjointFilesInEitherOrder(t *testing.T) {
	for _, real := range []bool{false, true} {
		for _, reverse := range []bool{false, true} {
			t.Run(fmt.Sprint(real, reverse), func(t *testing.T) {
				checkDisjointImportFiles(t, real, reverse)
			})
		}
	}
}

func checkDisjointImportFiles(t *testing.T, real, reverse bool) {
	t.Helper()
	p := people(t)
	if !real {
		p.f.c.People.Issues = p.f.store.native
		p.f.restart(t)
	}
	low, high := importRecord(7), importRecord(900)
	low.State, low.ClosedAt = model.IssueOpen, nil
	files := []apicontract.IssueImportArgs{importArgs(t, low), importArgs(t, high)}
	if reverse {
		files[0], files[1] = files[1], files[0]
	}
	for _, q := range files {
		if _, err := importCall(t, p, q); err != nil {
			t.Fatal(err)
		}
	}
	for _, q := range files {
		r, err := importCall(t, p, q)
		if err != nil || !r.AlreadyImported {
			t.Fatal(r, err)
		}
	}
	if _, err := importCall(t, p, importArgs(t, importRecord(11), high)); err == nil {
		t.Fatal("overlapping file accepted")
	}
	if _, err := p.f.s.PrepareIssueGet(t.Context(), adminCaller(), apicontract.IssueGetArgs{ID: 11}); err == nil {
		t.Fatal("overlapping file partially applied")
	}
	checkImportedRead(t, p, low)
	checkImportedRead(t, p, high)
	if v := createTask(t, p, "origin", "native"); v.ID != 901 {
		t.Fatal(v.ID)
	}
}

func TestIssueImportPreservesOrderedHistory(t *testing.T) {
	p := people(t)
	v := importRecord(7)
	v.Body = "Current body."
	v.History = []model.IssueHistory{
		{Kind: "append", At: v.CreatedAt, Author: v.Author, Text: strings.Repeat("a", 30000)},
		{Kind: "append", At: v.CreatedAt, Author: v.Author, Text: strings.Repeat("b", 30000)},
	}
	if _, err := importCall(t, p, importArgs(t, v)); err != nil {
		t.Fatal(err)
	}
	checkImportedRead(t, p, v)
}

func TestIssueImportSourceCloserSurvivesLocalTransitions(t *testing.T) {
	for _, real := range []bool{false, true} {
		t.Run(fmt.Sprint(real), func(t *testing.T) {
			p := people(t)
			if !real {
				p.f.c.People.Issues = p.f.store.native
				p.f.restart(t)
			}
			source := importRecord(42)
			if _, err := importCall(t, p, importArgs(t, source)); err != nil {
				t.Fatal(err)
			}
			reopen, err := p.f.s.PrepareIssueReopen(t.Context(), adminCaller(), apicontract.IssueReopenArgs{ID: 42})
			opened, err := execute(t.Context(), reopen, err)
			if err != nil || opened.Issue.ClosedAt != nil || opened.Issue.Closer != source.Closer {
				t.Fatal(opened, err)
			}
			closeCall, err := p.f.s.PrepareIssueClose(t.Context(), adminCaller(), apicontract.IssueCloseArgs{ID: 42})
			closed, err := execute(t.Context(), closeCall, err)
			if err != nil {
				t.Fatal(err)
			}
			v := closed.Issue
			if v.Closer != source.Closer || v.SourceClosedAt == nil || !v.SourceClosedAt.Equal(*source.ClosedAt) || v.ClosedAt.Equal(*v.SourceClosedAt) {
				t.Fatal("source closing attribution changed", v)
			}
		})
	}
}
