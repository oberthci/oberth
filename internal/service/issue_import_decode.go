package service

import (
	"bytes"
	"crypto/sha256"
	"fmt"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/strictjson"
)

// DecodeIssueImport uses the ordinary request and administrator-record bounds.
func DecodeIssueImport(q apicontract.IssueImportArgs) (stateport.IssueImport, error) {
	result := stateport.IssueImport{FileSHA256: fmt.Sprintf("%x", sha256.Sum256([]byte(q.JSONL))), DryRun: q.DryRun}
	if len(q.JSONL) > apicontract.RequestBytes {
		return result, stateport.ImportProblem(stateport.ImportFileSize, 1, 0, nil)
	}
	lines := bytes.Split([]byte(q.JSONL), []byte{'\n'})
	if len(lines) > 1 && len(lines[len(lines)-1]) == 0 {
		lines = lines[:len(lines)-1]
	}
	header, err := decodeImportHeader(lines[0])
	if err != nil {
		return result, err
	}
	if len(lines)-1 != header.Count {
		return result, stateport.ImportProblem(stateport.ImportCount, 1, 0, nil)
	}
	for i, line := range lines[1:] {
		record, err := decodeImportLine(line, result.Line(i))
		if err != nil {
			return result, err
		}
		result.Records = append(result.Records, record)
	}
	if err := result.Validate(); err != nil {
		return result, err
	}
	return result, nil
}

func decodeImportLine(line []byte, lineNumber int) (model.IssueImportRecord, error) {
	if len(line) > apicontract.AdminBytes {
		return model.IssueImportRecord{}, stateport.ImportProblem(stateport.ImportLineSize, lineNumber, 0, nil)
	}
	var record model.IssueImportRecord
	if err := strictjson.DecodeStrictObject(line, &record, apicontract.AdminBytes); err != nil {
		return model.IssueImportRecord{}, stateport.ImportContext(err, lineNumber, 0)
	}
	record.Defaults()
	return record, nil
}

func decodeImportHeader(line []byte) (model.IssueImportHeader, error) {
	var header model.IssueImportHeader
	if len(line) > apicontract.AdminBytes {
		return header, stateport.ImportProblem(stateport.ImportLineSize, 1, 0, nil)
	}
	if err := strictjson.DecodeStrictObject(line, &header, apicontract.AdminBytes); err != nil || header.Count < 1 {
		return header, stateport.ImportProblem(stateport.ImportHeader, 1, 0, err)
	}
	return header, nil
}
