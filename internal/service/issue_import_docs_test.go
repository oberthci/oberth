package service_test

import (
	"os"
	"testing"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/service"
)

func TestDocumentationIssueImportRecord(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile("../../docs/examples/issues-001.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	q, err := service.DecodeIssueImport(apicontract.IssueImportArgs{JSONL: string(raw)})
	if err != nil || len(q.Records) != 1 {
		t.Fatalf("copyable import: %v", err)
	}
	issue := q.Records[0]
	if issue.State != "closed" || issue.ClosedAt == nil || len(issue.History) != 2 {
		t.Fatal("example omits closed state or ordered history")
	}
	for field, value := range map[string]string{
		"body": issue.Body, "author": issue.Author, "closer": issue.Closer, "upstream": issue.Upstream,
	} {
		if value == "" {
			t.Errorf("example omits %s", field)
		}
	}
}

func TestDocumentationLossRecordIsNotImportInput(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile("../../docs/examples/losses.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.DecodeIssueImport(apicontract.IssueImportArgs{JSONL: string(raw)}); err == nil {
		t.Fatal("loss record admitted as an issue file")
	}
}
