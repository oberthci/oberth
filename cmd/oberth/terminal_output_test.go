package main

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func TestTerminalWriterPreservesOnlySGRAndText(t *testing.T) {
	input := "é\t\x1b[31mred\x1b[0m\n\x1b]52;c;secret\a\x1b[2J\r\b\x00\u009b2J\u202e\x1bPdata\x1b\\\xff\x1b["
	want := "é\t\x1b[31mred\x1b[0m\n\\x1b]52;c;secret\\a\\x1b[2J\\r\\b\\x00\\u009b2J\\u202e\\x1bPdata\\x1b\\\\xff\\x1b["
	for split := 0; split <= len(input); split++ {
		var out bytes.Buffer
		writer := &terminalWriter{out: &out}
		for _, text := range []string{input[:split], input[split:]} {
			if n, err := io.WriteString(writer, text); err != nil || n != len(text) {
				t.Fatal("write failed", n, err)
			}
		}
		if err := writer.Flush(); err != nil || out.String() != want {
			t.Fatalf("split=%d: %q, want %q: %v", split, out.String(), want, err)
		}
	}
}

func TestLogAndIssueUseOneTerminalWriter(t *testing.T) {
	text := "\x1b[31mtext\x1b[0m\x1b]title\a\r"
	for _, terminal := range []bool{false, true} {
		for _, args := range [][]string{{"log", "run", "build"}, {"issue", "get", "1"}} {
			b := fake.Bindings()
			b.RunGet.Handler = &fake.Handler[apicontract.RunGetArgs, apicontract.RunRecord]{Value: apicontract.RunRecord{RunSummary: stepmodel.RunSummary{ID: "run", Status: stepmodel.RunPassed, Steps: []stepmodel.RunStep{{Path: "build", Name: "build", Status: stepmodel.StepPassed}}}}}
			b.RunLogs.Handler = &fake.Handler[apicontract.RunLogsArgs, apicontract.LogResult]{Value: apicontract.LogResult{Content: text}}
			b.IssueGet.Handler = &fake.Handler[apicontract.IssueGetArgs, model.Issue]{Value: model.Issue{ID: 1, Title: "title", Body: text}}
			app, out, diagnostic := fixture(t, b)
			if terminal {
				app.out = &terminalWriter{out: out}
			}
			if code := app.run(t.Context(), args); code != 0 {
				t.Fatal(args, code, diagnostic.String())
			}
			want := text
			if terminal {
				want = "\x1b[31mtext\x1b[0m\\x1b]title\\a\\r"
			}
			if !strings.Contains(out.String(), want) {
				t.Fatalf("terminal=%v %v output=%q", terminal, args, out.String())
			}
		}
	}
}

func TestOutputPipeBytesAreUnchanged(t *testing.T) {
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer read.Close()
	data := []byte("\x1b]52;c;raw\a\r\x00\xff")
	if out := terminalOutput(write); out != write {
		t.Fatal("pipe was wrapped")
	}
	if _, err = terminalOutput(write).Write(data); err != nil {
		t.Fatal(err)
	}
	_ = write.Close()
	got, err := io.ReadAll(read)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatal("pipe bytes changed", got, err)
	}
}
