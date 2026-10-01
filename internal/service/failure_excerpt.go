package service

import (
	"bytes"
	"strings"

	"github.com/oberthci/oberth/internal/runlog"
)

// failureDiagnosticPattern selects the lines of a failed step's own log slice
// that most often name the root cause (Go runtime crashes, panics, test
// failures, tool errors). They are kept ahead of the slice's tail when they
// scrolled out of it.
const failureDiagnosticPattern = `fatal error|panic|FAIL|Error:`

const (
	// excerptTailLines bounds the tail read; the byte budget below trims it.
	excerptTailLines = 400
	// excerptDiagnosticLines bounds how many earlier diagnostic lines are
	// considered; the newest that fit the diagnostic budget are kept.
	excerptDiagnosticLines = 64
)

// failureExcerpt returns the bounded CI-issue excerpt for a failed run.
//
// When the run recorded a failed burn, the excerpt comes only from that
// burn/step's own named log slice — exactly what `logs <sha> <step>` or
// `run_logs <id> <burn> <step>` returns — never from the combined run log: an
// Argo DAG leaf can fail while sibling leaves keep logging, and the combined
// tail then shows only their output (#656). If the failed step has no slice
// (it never wrote a line), the excerpt is empty and the issue keeps the run
// error and the full-log pointer rather than another step's output. Only a
// run without any recorded failed step (a Job-level failure) falls back to the
// combined run-log tail.
func (scheduler *Scheduler) failureExcerpt(runID, burn, step string) ([]byte, error) {
	burn, step = strings.TrimSpace(burn), strings.TrimSpace(step)
	if burn == "" {
		return scheduler.logs.Tail(runID, maximumCIIssueTailBytes)
	}
	return stepFailureExcerpt(scheduler.logs, runID, burn, step, maximumCIIssueTailBytes), nil
}

type excerptLine struct {
	number int
	text   string
}

// stepFailureExcerpt composes at most budget bytes from one step's slice: the
// newest earlier lines matching failureDiagnosticPattern that fall before the
// tail window (at most a quarter of the budget), a blank separator line, then
// the slice's tail. Every non-empty line is a line of that slice, carrying its
// own "[burn/step] " prefix. A missing or unreadable slice yields nil.
func stepFailureExcerpt(logs LogStore, runID, burn, step string, budget int64) []byte {
	if budget <= 0 {
		return nil
	}
	tailBody, tailMeta, err := logs.ReadFiltered(runID, burn, step, runlog.Filter{Tail: true, Limit: excerptTailLines})
	if err != nil {
		return nil
	}
	tail := numberedExcerptLines(tailBody, tailMeta.LineNumbers)
	if len(tail) == 0 {
		return nil
	}
	var diagnostics []excerptLine
	if body, meta, diagnosticErr := logs.ReadFiltered(runID, burn, step, runlog.Filter{
		Pattern: failureDiagnosticPattern, Tail: true, Limit: excerptDiagnosticLines,
	}); diagnosticErr == nil {
		diagnostics = numberedExcerptLines(body, meta.LineNumbers)
	}

	window := newestWithin(tail, budget)
	earlier := linesBefore(diagnostics, firstNumber(window))
	if len(earlier) == 0 {
		return joinExcerptLines(nil, window)
	}
	reserve := min(budget/4, totalBytes(earlier)+1)
	window = newestWithin(tail, budget-reserve)
	kept := newestWithin(linesBefore(diagnostics, firstNumber(window)), reserve-1)
	if len(kept) == 0 {
		return joinExcerptLines(nil, newestWithin(tail, budget))
	}
	return joinExcerptLines(kept, window)
}

// numberedExcerptLines pairs a filtered read with the slice line numbers the
// runlog reported for it. A count mismatch means the body cannot be trusted
// to align with the numbering, so nothing is returned.
func numberedExcerptLines(body []byte, numbers []int) []excerptLine {
	if len(body) == 0 {
		return nil
	}
	parts := bytes.SplitAfter(body, []byte("\n"))
	if last := len(parts) - 1; last >= 0 && len(parts[last]) == 0 {
		parts = parts[:last]
	}
	if len(parts) != len(numbers) {
		return nil
	}
	lines := make([]excerptLine, 0, len(parts))
	for index, part := range parts {
		text := strings.ToValidUTF8(string(part), "�")
		if !strings.HasSuffix(text, "\n") {
			text += "\n"
		}
		lines = append(lines, excerptLine{number: numbers[index], text: text})
	}
	return lines
}

// newestWithin keeps the newest whole lines whose total size fits budget. If
// even the newest line alone is larger, its last budget bytes are kept so the
// final error text is never dropped entirely.
func newestWithin(lines []excerptLine, budget int64) []excerptLine {
	if budget <= 0 || len(lines) == 0 {
		return nil
	}
	var used int64
	first := len(lines)
	for index := len(lines) - 1; index >= 0; index-- {
		size := int64(len(lines[index].text))
		if used+size > budget {
			break
		}
		used += size
		first = index
	}
	if first == len(lines) {
		last := lines[len(lines)-1]
		text := strings.ToValidUTF8(last.text[int64(len(last.text))-budget:], "")
		return []excerptLine{{number: last.number, text: text}}
	}
	return lines[first:]
}

func linesBefore(lines []excerptLine, number int) []excerptLine {
	var before []excerptLine
	for _, line := range lines {
		if line.number < number {
			before = append(before, line)
		}
	}
	return before
}

func firstNumber(lines []excerptLine) int {
	if len(lines) == 0 {
		return 0
	}
	return lines[0].number
}

func totalBytes(lines []excerptLine) int64 {
	var total int64
	for _, line := range lines {
		total += int64(len(line.text))
	}
	return total
}

func joinExcerptLines(diagnostics, tail []excerptLine) []byte {
	var out bytes.Buffer
	for _, line := range diagnostics {
		out.WriteString(line.text)
	}
	if len(diagnostics) > 0 {
		out.WriteString("\n")
	}
	for _, line := range tail {
		out.WriteString(line.text)
	}
	return out.Bytes()
}
