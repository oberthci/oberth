package main

import (
	"bytes"
	"io"
	"os"
	"slices"
	"strings"
	"syscall"
	"unicode/utf8"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/client"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stateport"
)

func importIssues(s *flowSession, args []string) (err error) {
	flags := flowFlags("issue import", s.app.home())
	rest, err := parseFlow(flags, args)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			p := publicerr.Classify(err)
			p.Context = append(p.Context, "File: "+rest[0])
			err = p
		}
	}()
	data, err := readIssueImportFile(rest[0])
	if err != nil {
		return err
	}
	q := apicontract.IssueImportArgs{JSONL: string(data), Upstream: *flagValue[string](flags, "upstream"), DryRun: *flagValue[bool](flags, "dry-run")}
	var r stateport.IssueImportResult
	if err = s.invoke("issue_import", q, &r); err != nil {
		return issueImportFailure(err)
	}
	return reportIssueImport(s, r)
}

func reportIssueImport(s *flowSession, r stateport.IssueImportResult) error {
	if r.AlreadyImported {
		if r.DryRun {
			return s.fact("already imported: %d issues, %d history entries. No issues imported.", r.Count, r.HistoryEntries)
		}
		return s.fact("already imported: %d issues, %d history entries", r.Count, r.HistoryEntries)
	}
	if r.DryRun {
		return s.fact("dry run: %d issues, %d history entries. No issues imported.", r.Count, r.HistoryEntries)
	}
	return s.fact("imported: %d issues, %d history entries", r.Count, r.HistoryEntries)
}

func readIssueImportFile(path string) ([]byte, error) {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, importFileError(err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, importFileError(err)
	}
	data, err := io.ReadAll(io.LimitReader(file, apicontract.RequestBytes+1))
	if err != nil {
		return nil, importFileError(err)
	}
	if len(data) > apicontract.RequestBytes {
		return nil, stateport.ImportProblem(stateport.ImportFileSize, 1, 0, nil)
	}
	// JSON string encoding replaces invalid UTF-8; refuse it before transport.
	for i, line := range bytes.Split(data, []byte{'\n'}) {
		if !utf8.Valid(line) {
			return nil, stateport.ImportProblem(stateport.ImportUTF8, i+1, 0, nil)
		}
	}
	return data, nil
}

func importFileError(cause error) error {
	return stateport.ImportContext(publicerr.New(publicerr.InvalidInput, "Cannot read a bounded issue import file.", "Select a readable regular JSON Lines file within the request limit.", cause), 1, 0)
}

func issueImportFailure(err error) error {
	p := publicerr.Classify(err)
	if client.IsOutcomeUnknown(err) {
		p.Context = append(p.Context, "Import outcome is uncertain; run the same file again to confirm already imported.")
	} else if !slices.ContainsFunc(p.Context, func(v string) bool {
		return strings.Contains(v, "Import outcome")
	}) {
		return stateport.ImportContext(p, 1, 0)
	}
	return p
}
