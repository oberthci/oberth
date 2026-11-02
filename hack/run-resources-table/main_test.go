package main

import (
	"compress/gzip"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAnonymousTableRefusesUnlabelledInputAndWithholdsAmbiguousAdvice(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(dir, "usage.jsonl.gz")
	output := filepath.Join(dir, "table.md")
	write := func(v any) {
		f, e := os.Create(input)
		if e != nil {
			t.Fatal(e)
		}
		z := gzip.NewWriter(f)
		if e = json.NewEncoder(z).Encode(v); e != nil {
			t.Fatal(e)
		}
		if e = z.Close(); e != nil {
			t.Fatal(e)
		}
		if e = f.Close(); e != nil {
			t.Fatal(e)
		}
	}
	write(map[string]any{})
	if e := run(input, output); e == nil {
		t.Fatal("accepted missing anonymous labels")
	}
	write(map[string]any{"repo": "repo-01", "run": "abcdef123456", "Burn": "step-abc12345", "Step": "step-abc12345", "MeasuredUsage": []any{map[string]any{"version": 2, "source": "kubelet-cadvisor", "memory_peak_bytes": 123, "samples": 4}}, "Termination": map[string]any{"container": map[string]any{"message": "private-value"}}})
	if e := run(input, output); e != nil {
		t.Fatal(e)
	}
	raw, e := os.ReadFile(output)
	if e != nil {
		t.Fatal(e)
	}
	s := string(raw)
	if !strings.Contains(s, "repo-01") || !strings.Contains(s, "step-abc12345") || !strings.Contains(s, "ambiguous") || strings.Contains(s, "private-value") {
		t.Fatal(s)
	}
}
