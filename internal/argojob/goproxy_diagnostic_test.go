package argojob

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/runprogress"
)

const proxyFailureLine = `Get "https://mirror.example.test:8444/public.example.test/module/@v/v1.0.0.zip": dial tcp 10.0.0.1:8444: connect: connection refused`

func TestGoProxyRefusalLogAttribution(t *testing.T) {
	for _, test := range []struct {
		name, proxy, line string
		want              bool
	}{
		{"proxy refusal", "https://mirror.example.test:8444", proxyFailureLine, true},
		{"disabled", "", proxyFailureLine, false},
		{"different proxy", "https://other.example.test:8444", proxyFailureLine, false},
		{"lookalike host", "https://mirror.example.test", strings.ReplaceAll(proxyFailureLine, ":8444", ".attacker.test"), false},
		{"tls failure", "https://mirror.example.test:8444", `Get "https://mirror.example.test:8444/module": tls: failed to verify certificate`, false},
		{"checksum failure", "https://mirror.example.test:8444", `https://mirror.example.test:8444/module: SECURITY ERROR checksum mismatch`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			var output bytes.Buffer
			step := StepResult{Burn: "build", Step: "image", Status: runprogress.StepFailed}
			// Replay retains the original line and emits one attributed diagnostic.
			if err := copyPrefixed(&output, step, strings.NewReader(test.line+"\n"+test.line+"\n"), test.proxy); err != nil {
				t.Fatal(err)
			}
			wantCount := 0
			if test.want {
				wantCount = 1
			}
			if count := strings.Count(output.String(), "[build/image] oberth: Go module proxy unavailable"); count != wantCount {
				t.Fatalf("diagnostics=%d want=%d: %s", count, wantCount, &output)
			}
			if !strings.Contains(output.String(), test.line) || step.Status != runprogress.StepFailed {
				t.Fatal("diagnostic changed command output or verdict")
			}
			// The live path applies the same annotation after timestamp dedup.
			output.Reset()
			logs := newStepLogFollowers(context.Background(), nil, &output, nil)
			defer logs.close()
			logs.goProxyURL = test.proxy
			worker := &stepLogFollower{owner: logs, step: step}
			input := "2026-10-26T17:17:40Z " + test.line + "\n2026-10-26T17:17:41Z " + test.line + "\n"
			if err := worker.consume(strings.NewReader(input)); err != nil {
				t.Fatal(err)
			}
			if err := worker.consume(strings.NewReader(input)); err != nil {
				t.Fatal(err)
			}
			if count := strings.Count(output.String(), "[build/image] oberth: Go module proxy unavailable"); count != wantCount {
				t.Fatalf("live diagnostics=%d want=%d: %s", count, wantCount, &output)
			}
		})
	}
}
