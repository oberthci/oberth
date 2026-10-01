package api

import (
	"regexp"
	"strconv"
	"strings"
	"testing"
)

/*
The dashboard log viewer colours lines client-side in logLineClass(). There is
no JavaScript runtime in the runner image and adding one would mean a pinned
Node fetch for a colour heuristic, so these tests drive the *shipped* patterns
instead of a copy of them: every regex is extracted from the embedded app.js,
compiled with Go's regexp, and replayed against log lines captured verbatim
from real runs on this server.

That covers the realistic failure mode — someone widens a pattern and a green
suite turns red again. It does not execute the JavaScript itself, so the
precedence order is pinned separately by TestLogLineClassPrecedenceOrder.
Keep classify() below in step with the JS function when the order changes.
*/

// logRuleSources pulls the LOG_RULES pattern table out of the embedded app.js.
func logRuleSources(t *testing.T) map[string]string {
	t.Helper()
	script, ok := staticAssets["app.js"]
	if !ok {
		t.Fatal("app.js is not embedded")
	}
	source := string(script.body)
	start := strings.Index(source, "const LOG_RULES = {")
	if start < 0 {
		t.Fatal("app.js no longer declares LOG_RULES; the log classifier lost its rule table")
	}
	end := strings.Index(source[start:], "\n};")
	if end < 0 {
		t.Fatal("LOG_RULES table is not terminated")
	}
	entry := regexp.MustCompile(`(?m)^\s*([A-Za-z]+):\s*("(?:[^"\\]|\\.)*"),\s*$`)
	rules := map[string]string{}
	for _, match := range entry.FindAllStringSubmatch(source[start:start+end], -1) {
		pattern, err := strconv.Unquote(match[2])
		if err != nil {
			t.Fatalf("LOG_RULES[%s] is not a decodable string literal: %v", match[1], err)
		}
		rules[match[1]] = pattern
	}
	if len(rules) == 0 {
		t.Fatal("LOG_RULES parsed to zero patterns")
	}
	return rules
}

// logSetValues pulls a `const NAME = new Set([...])` literal out of app.js.
func logSetValues(t *testing.T, name string) map[string]bool {
	t.Helper()
	source := string(staticAssets["app.js"].body)
	start := strings.Index(source, "const "+name+" = new Set([")
	if start < 0 {
		t.Fatalf("app.js no longer declares %s", name)
	}
	end := strings.Index(source[start:], "]);")
	if end < 0 {
		t.Fatalf("%s is not terminated", name)
	}
	values := map[string]bool{}
	for _, match := range regexp.MustCompile(`"((?:[^"\\]|\\.)*)"`).FindAllStringSubmatch(source[start:start+end], -1) {
		unquoted, err := strconv.Unquote(`"` + match[1] + `"`)
		if err != nil {
			t.Fatalf("%s holds an undecodable literal: %v", name, err)
		}
		values[unquoted] = true
	}
	return values
}

// TestLogLineClassPatternsAreRE2Compatible fails if a pattern gains a
// lookahead, lookbehind or backreference. Those work in the browser but make
// the rules untestable here, which is how the naive heuristic survived so long.
func TestLogLineClassPatternsAreRE2Compatible(t *testing.T) {
	t.Parallel()
	for name, pattern := range logRuleSources(t) {
		if _, err := regexp.Compile(pattern); err != nil {
			t.Errorf("LOG_RULES[%s] is not RE2-compatible: %v", name, err)
		}
	}
}

// TestLogLineClassPrecedenceOrder pins the sequence the JS applies its rules
// in. Precision depends on it: a Go test verdict must be decided before the
// free-text scan can read the test's name, and a structured error= field must
// be judged before its key name reaches that scan.
func TestLogLineClassPrecedenceOrder(t *testing.T) {
	t.Parallel()
	source := string(staticAssets["app.js"].body)
	start := strings.Index(source, "function logLineClass(line) {")
	end := strings.Index(source, "\nfunction splitLogText")
	if start < 0 || end <= start {
		t.Fatal("app.js is missing logLineClass")
	}
	body := source[start:end]
	if strings.Contains(body, ".toLowerCase().includes(") || strings.Contains(body, `l.includes("error")`) {
		t.Fatal("logLineClass reverted to a bare substring scan")
	}
	order := []string{
		"stepPrefix", // classification-only prefix strip
		"markBad", "markOk", "markWarn",
		"hardBad",
		"testFail", "testSkip", "testPass", "testQuiet",
		"errField",
		"levelField",
		"wordBad", "wordOk", "wordWarn",
		"info",
	}
	at := -1
	for _, name := range order {
		next := strings.Index(body, "LOG_RE."+name)
		if next < 0 {
			t.Fatalf("logLineClass no longer applies the %s rule", name)
		}
		if next <= at {
			t.Fatalf("logLineClass applies %s out of order; precision depends on the documented precedence", name)
		}
		at = next
	}
	if !strings.Contains(body, "LOG_ERR_FIELD_ALL") {
		t.Fatal("logLineClass no longer strips structured error fields before the free-text scan")
	}
}

// TestLogLineClassNilSentinels keeps the zero-value vocabulary intact. Losing
// "<nil>" alone reddens every routine sub-process line the runner emits.
func TestLogLineClassNilSentinels(t *testing.T) {
	t.Parallel()
	nils := logSetValues(t, "LOG_NIL_VALUES")
	for _, want := range []string{"<nil>", "nil", "null", "none", `""`, "''", ""} {
		if !nils[want] {
			t.Errorf("LOG_NIL_VALUES lost the %q sentinel; error=%s would be reported as a failure", want, want)
		}
	}
	levels := logSetValues(t, "LOG_LEVEL_BAD")
	for _, want := range []string{"error", "fatal"} {
		if !levels[want] {
			t.Errorf("LOG_LEVEL_BAD lost %q; level=%s would stop being reported as a failure", want, want)
		}
	}
}

// classify mirrors logLineClass's precedence, driven entirely by the patterns
// and value sets extracted from the shipped app.js.
func classify(t *testing.T, line string) string {
	t.Helper()
	rules := logRuleSources(t)
	compiled := map[string]*regexp.Regexp{}
	for name, pattern := range rules {
		compiled[name] = regexp.MustCompile("(?i)" + pattern)
	}
	nils := logSetValues(t, "LOG_NIL_VALUES")
	levelBad := logSetValues(t, "LOG_LEVEL_BAD")
	levelWarn := logSetValues(t, "LOG_LEVEL_WARN")

	body := strings.TrimSpace(compiled["stepPrefix"].ReplaceAllString(line, ""))
	if body == "" {
		return ""
	}
	for _, rule := range []struct{ name, class string }{
		{"markBad", "bad"}, {"markOk", "ok"}, {"markWarn", "warn"},
		{"hardBad", "bad"},
		{"testFail", "bad"}, {"testSkip", "warn"}, {"testPass", "ok"}, {"testQuiet", ""},
	} {
		if compiled[rule.name].MatchString(body) {
			return rule.class
		}
	}
	if err := compiled["errField"].FindStringSubmatch(body); err != nil {
		if !nils[strings.ToLower(err[3])] {
			return "bad"
		}
	}
	if level := compiled["levelField"].FindStringSubmatch(body); level != nil {
		name := strings.ToLower(level[3])
		if levelBad[name] {
			return "bad"
		}
		if levelWarn[name] {
			return "warn"
		}
		return ""
	}
	residual := compiled["errField"].ReplaceAllString(body, " ")
	for _, rule := range []struct{ name, class string }{
		{"wordBad", "bad"}, {"wordOk", "ok"}, {"wordWarn", "warn"},
	} {
		if compiled[rule.name].MatchString(residual) {
			return rule.class
		}
	}
	if compiled["info"].MatchString(body) {
		return "info"
	}
	return ""
}

// TestLogLineClassifiesRealRunOutput replays lines captured verbatim from runs
// on this server. The "must not redden" half is the regression that prompted
// this: a naive contains("error") painted a fully green suite red.
func TestLogLineClassifiesRealRunOutput(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		line  string
		class string
	}{
		// Routine success lines that merely contain the word.
		{"argo nil error", `time=2026-08-14T23:19:20.845Z level=INFO msg="sub-process exited" argo=true error=<nil>`, ""},
		{"argo nil error mid-line", `time=2026-08-14T22:19:53.874Z level=INFO msg="sub-process exited" error=<nil> argo=true`, ""},
		{"module named errors", "go: downloading github.com/go-errors/errors v1.5.1", ""},
		{"module go-openapi errors", "go: downloading github.com/go-openapi/errors v0.22.8", ""},
		{"module go-errorlint", "go: downloading codeberg.org/polyfloyd/go-errorlint v1.9.0", ""},
		{"module multierror", "go: downloading github.com/hashicorp/go-multierror v1.1.1", ""},
		{"module xerrors", "go: downloading golang.org/x/xerrors v0.0.0-20240903120638-7835f813f4da", ""},
		{"module pkg errors", "go: downloading github.com/pkg/errors v0.9.1", ""},
		{"unrelated module", "go: downloading github.com/cloudflare/circl v1.6.3", ""},
		// Go test output: the verdict is the marker, never the test's name.
		{"pass with Errors in name", "--- PASS: TestValidateUsageErrors (0.00s)", ""},
		{"pass with Failed in name", "--- PASS: TestExitCodeNeverReportsSuccessForAFailedNode (0.00s)", ""},
		{"pass with Unavailable in name", "--- PASS: TestOpenStartupDatabasePropagatesWitnessUnavailableWithIntactDatabase (0.44s)", ""},
		{"indented subtest pass", "    --- PASS: TestSecretStoreVerifyFailsClosed/missing_path_fails_with_the_store_error_and_hints (0.02s)", ""},
		{"run with error in name", "=== RUN   TestExchangeErrorNeverContainsParentToken", ""},
		{"pause with error in name", "=== PAUSE TestValidateUsageErrors", ""},
		{"cont with error in name", "=== CONT  TestValidateUsageErrors", ""},
		{"subtest named helm_error", "=== RUN   TestFindHelmRelease/helm_error", ""},
		// Genuine failures must still catch.
		{"cobra error prefix", "Error: exit status 1", "bad"},
		{"prefixed cobra error", "[release-build/release-build] Error: exit status 1", "bad"},
		{"non-empty error field", `time=2026-08-14T23:24:33.761Z level=INFO msg="sub-process exited" argo=true error="exit status 1"`, "bad"},
		{"test failure", "--- FAIL: TestOberthPipelinesMountTheServerProvidedSource (0.08s)", "bad"},
		{"package failure", "FAIL\tgithub.com/oberthci/oberth/internal/argojob\t0.972s", "bad"},
		{"bare FAIL", "FAIL", "bad"},
		{"panic", "panic: runtime error: index out of range [3] with length 2", "bad"},
		{"data race", "WARNING: DATA RACE", "bad"},
		{"git fatal", "fatal: could not read from remote repository", "bad"},
		{"level=ERROR", `time=2026-08-14T23:24:33.761Z level=ERROR msg="publication failed"`, "bad"},
		// Package-level success and warnings keep their signal.
		{"package ok", "ok  \tgithub.com/oberthci/oberth\t2.029s", "ok"},
		{"bare PASS", "PASS", "ok"},
		{"trivy warn", "2026-08-14T23:19:20Z\tWARN\tUsing severities from other vendors for some vulnerabilities.", "warn"},
		{"skipped test", "--- SKIP: TestLiveRepositoryBuildPipelineExecutes (0.00s)", "warn"},
		// Lines the dashboard writes itself.
		{"summary fail marker", "[fail] failed at release-build / release-build — 12 steps passed", "bad"},
		{"summary pass marker", "[pass] 42 steps passed across 6 burns", "ok"},
		{"summary pending marker", "[pending] running in phase ci — 8 passed so far", "warn"},
		{"summary run error", "error: release tag is not reachable from the default branch", "bad"},
		{"summary command", "$ oberth run oberth/main", "info"},
		{"step header passed", "step: setup/tools-dir · passed", "ok"},
		{"step header failed", "step: release-build/release-build · failed · exit 1", "bad"},
		{"step log unavailable", "step log unavailable: network error", "bad"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			if got := classify(t, testCase.line); got != testCase.class {
				t.Errorf("classified as %q, want %q\n  line: %s", got, testCase.class, testCase.line)
			}
		})
	}
}
