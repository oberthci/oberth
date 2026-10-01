package runlog

import (
	"bufio"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/redact"
)

func seedRun(t *testing.T, runID string, lines []string) *Store {
	t.Helper()
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	file, err := store.Create(runID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(strings.Join(lines, "\n") + "\n"); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.BuildIndex(runID); err != nil {
		t.Fatal(err)
	}
	return store
}

func numberedStep(prefix string, count int) []string {
	lines := make([]string, 0, count)
	for i := 1; i <= count; i++ {
		lines = append(lines, prefix+" line "+strconv.Itoa(i))
	}
	return lines
}

func TestZeroFilterReturnsTheSameBytesAsAnUnfilteredRead(t *testing.T) {
	store := seedRun(t, "r-zero", numberedStep("[test/unit]", 12))

	body, meta, err := store.ReadFiltered("r-zero", "test", "unit", Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(body), "\n"); got != 12 {
		t.Fatalf("zero filter returned %d lines, want all 12", got)
	}
	if !strings.Contains(string(body), "[test/unit] line 1\n") {
		t.Fatalf("zero filter dropped the step prefix: %q", body)
	}
	if meta.TotalLines != 12 || meta.ReturnedLines != 12 || meta.Truncated {
		t.Fatalf("meta = %#v, want all 12 lines untruncated", meta)
	}
}

func TestOffsetAndLimitPageThroughAStepDeterministically(t *testing.T) {
	store := seedRun(t, "r-page", numberedStep("[test/unit]", 20))

	first, _, err := store.ReadFiltered("r-page", "test", "unit", Filter{Offset: 0, Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	second, meta, err := store.ReadFiltered("r-page", "test", "unit", Filter{Offset: 5, Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(first), "line 1\n") || strings.Contains(string(first), "line 6\n") {
		t.Fatalf("first page = %q", first)
	}
	if !strings.Contains(string(second), "line 6\n") || strings.Contains(string(second), "line 5\n") {
		t.Fatalf("second page = %q", second)
	}
	if meta.ReturnedLines != 5 || meta.TotalLines != 20 || !meta.Truncated {
		t.Fatalf("meta = %#v, want 5 of 20 truncated", meta)
	}
}

func TestTailReturnsTheLastLines(t *testing.T) {
	store := seedRun(t, "r-tail", numberedStep("[test/unit]", 20))

	body, meta, err := store.ReadFiltered("r-tail", "test", "unit", Filter{Tail: true, Limit: 3})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "line 20\n") || strings.Contains(string(body), "line 17\n") {
		t.Fatalf("tail = %q, want the last three lines", body)
	}
	if meta.ReturnedLines != 3 || meta.TotalLines != 20 {
		t.Fatalf("meta = %#v", meta)
	}
}

func TestFilteredLimitBoundsLinesIncludingContext(t *testing.T) {
	lines := []string{}
	for i := 0; i < 20; i++ {
		lines = append(lines, "[t/u] PASS case", "[t/u] ok package", "[t/u] context")
	}
	store := seedRun(t, "r-filter-limit", lines)
	for _, tail := range []bool{false, true} {
		for _, context := range []int{0, 1, 3} {
			body, meta, err := store.ReadFiltered("r-filter-limit", "t", "u", Filter{Pattern: "^(PASS|ok)", Context: context, Limit: 8, Tail: tail})
			if err != nil {
				t.Fatal(err)
			}
			if meta.ReturnedLines != 8 || strings.Count(string(body), "\n") != 8 || !meta.Truncated || meta.MatchedLines != 40 || meta.TotalLines != 60 {
				t.Errorf("tail=%t context=%d: meta=%#v body=%q", tail, context, meta, body)
			}
			if tail && meta.LineNumbers[len(meta.LineNumbers)-1] < 59 {
				t.Errorf("tail lost final matches: %v", meta.LineNumbers)
			}
		}
	}
}

func TestFilteredTailKeepsNewestLinesPastByteBudget(t *testing.T) {
	raw := strings.Repeat("[t/u] PASS "+strings.Repeat("x", 1024)+"\n", 5000) + "[t/u] PASS final\n"
	body, meta, err := FilterBytes([]byte(raw), Filter{Pattern: "PASS", Tail: true, Limit: 8})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(string(body), "[t/u] PASS final\n") || meta.ReturnedLines != 8 || !meta.Truncated {
		t.Fatalf("tail lost newest matches: %#v", meta)
	}
}

func TestMetaReportsTheTrueTotalWhenLimitTruncates(t *testing.T) {
	store := seedRun(t, "r-meta", numberedStep("[test/unit]", 200))

	_, meta, err := store.ReadFiltered("r-meta", "test", "unit", Filter{Limit: 3})
	if err != nil {
		t.Fatal(err)
	}
	if meta.ReturnedLines != 3 {
		t.Fatalf("returned %d lines, want 3", meta.ReturnedLines)
	}
	if meta.TotalLines != 200 {
		t.Fatalf("meta.TotalLines = %d, want the true 200 rather than the returned count", meta.TotalLines)
	}
	if meta.MatchedLines != 200 {
		t.Fatalf("meta.MatchedLines = %d, want 200 when no pattern narrows the step", meta.MatchedLines)
	}
	if !meta.Truncated {
		t.Fatal("meta.Truncated is false while 197 lines were withheld")
	}
}

func TestOffsetBeyondTheStepReturnsNothingWithoutError(t *testing.T) {
	store := seedRun(t, "r-past", numberedStep("[test/unit]", 4))

	body, meta, err := store.ReadFiltered("r-past", "test", "unit", Filter{Offset: 99})
	if err != nil {
		t.Fatal(err)
	}
	if len(body) != 0 || meta.ReturnedLines != 0 {
		t.Fatalf("body = %q meta = %#v, want an empty page", body, meta)
	}
	if meta.TotalLines != 4 {
		t.Fatalf("meta.TotalLines = %d, want 4", meta.TotalLines)
	}
}

func TestFilterSpansEveryRangeOfAMultiBurnStep(t *testing.T) {
	store := seedRun(t, "r-multi", []string{
		"[build/compile] first",
		"[test/unit] alpha",
		"[build/compile] second",
		"[test/unit] beta",
	})

	body, meta, err := store.ReadFiltered("r-multi", "build", "compile", Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "first") || !strings.Contains(string(body), "second") {
		t.Fatalf("body = %q, want both compile ranges", body)
	}
	if strings.Contains(string(body), "alpha") {
		t.Fatalf("body = %q, leaked another step", body)
	}
	if meta.TotalLines != 2 {
		t.Fatalf("meta.TotalLines = %d, want 2 across both ranges", meta.TotalLines)
	}
}

func TestEmptyFilterDoesNotTruncateAStepWithManyShortLines(t *testing.T) {
	const many = 100500
	lines := make([]string, 0, many)
	for i := 0; i < many; i++ {
		lines = append(lines, "[t/u] x")
	}
	store := seedRun(t, "r-short", lines)

	_, meta, err := store.ReadFiltered("r-short", "t", "u", Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if meta.Truncated {
		t.Fatalf("an unfiltered read truncated at the default line limit; meta = %#v", meta)
	}
	if meta.ReturnedLines != meta.TotalLines {
		t.Fatalf("returned %d of %d lines with no filter applied", meta.ReturnedLines, meta.TotalLines)
	}
}

func TestPatternReturnsOnlyMatchingLines(t *testing.T) {
	store := seedRun(t, "r-pat", []string{
		"[test/unit] starting",
		"[test/unit] ok TestAlpha",
		"[test/unit] FAIL TestBeta",
		"[test/unit] ok TestGamma",
		"[test/unit] FAIL TestDelta",
	})

	body, meta, err := store.ReadFiltered("r-pat", "test", "unit", Filter{Pattern: "FAIL"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "TestAlpha") || strings.Contains(string(body), "starting") {
		t.Fatalf("body = %q, want only failures", body)
	}
	if !strings.Contains(string(body), "TestBeta") || !strings.Contains(string(body), "TestDelta") {
		t.Fatalf("body = %q, want both failures", body)
	}
	if meta.TotalLines != 5 || meta.MatchedLines != 2 || meta.ReturnedLines != 2 {
		t.Fatalf("meta = %#v, want 2 matched of 5", meta)
	}
}

func TestPatternMatchesAgainstTheContentNotTheStepPrefix(t *testing.T) {
	store := seedRun(t, "r-anchor", []string{
		"[test/unit] go test ./...",
		"[test/unit] all good",
	})

	body, meta, err := store.ReadFiltered("r-anchor", "test", "unit", Filter{Pattern: "^go test"})
	if err != nil {
		t.Fatal(err)
	}
	if meta.MatchedLines != 1 {
		t.Fatalf("^ anchored against the [burn/step] prefix instead of the content; meta = %#v", meta)
	}
	if !strings.Contains(string(body), "[test/unit] go test") {
		t.Fatalf("body = %q, want the raw line including its prefix", body)
	}
}

func TestContextSurroundsEachMatchWithoutDuplicatingLines(t *testing.T) {
	store := seedRun(t, "r-ctx", []string{
		"[t/u] one", "[t/u] two", "[t/u] BOOM", "[t/u] four", "[t/u] five", "[t/u] BOOM", "[t/u] seven",
	})

	body, meta, err := store.ReadFiltered("r-ctx", "t", "u", Filter{Pattern: "BOOM", Context: 1})
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	for _, want := range []string{"two", "BOOM", "four", "five", "seven"} {
		if !strings.Contains(text, want) {
			t.Fatalf("body = %q, missing %q", text, want)
		}
	}
	if strings.Contains(text, "one") {
		t.Fatalf("body = %q, leaked a line outside every context window", text)
	}
	if got := strings.Count(text, "five"); got != 1 {
		t.Fatalf("line 5 appears %d times; overlapping context windows duplicated it", got)
	}
	if meta.MatchedLines != 2 {
		t.Fatalf("meta.MatchedLines = %d, want 2 matches rather than emitted lines", meta.MatchedLines)
	}
}

func TestLineNumbersSurviveFiltering(t *testing.T) {
	store := seedRun(t, "r-nums", []string{
		"[t/u] a", "[t/u] b", "[t/u] FAIL c", "[t/u] d", "[t/u] FAIL e",
	})

	_, meta, err := store.ReadFiltered("r-nums", "t", "u", Filter{Pattern: "FAIL"})
	if err != nil {
		t.Fatal(err)
	}
	if len(meta.LineNumbers) != 2 || meta.LineNumbers[0] != 3 || meta.LineNumbers[1] != 5 {
		t.Fatalf("meta.LineNumbers = %v, want the original positions [3 5]", meta.LineNumbers)
	}
}

func TestOffsetAndLimitPageThroughMatchesNotRawLines(t *testing.T) {
	lines := []string{}
	for i := 1; i <= 10; i++ {
		lines = append(lines, "[t/u] filler")
		lines = append(lines, "[t/u] FAIL case"+strconv.Itoa(i))
	}
	store := seedRun(t, "r-mpage", lines)

	body, meta, err := store.ReadFiltered("r-mpage", "t", "u", Filter{Pattern: "FAIL", Offset: 2, Limit: 3})
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	if !strings.Contains(text, "case3") || !strings.Contains(text, "case5") {
		t.Fatalf("body = %q, want the third through fifth match", text)
	}
	if strings.Contains(text, "case2") || strings.Contains(text, "case6") {
		t.Fatalf("body = %q, paged over raw lines instead of matches", text)
	}
	if meta.MatchedLines != 10 || meta.ReturnedLines != 3 || !meta.Truncated {
		t.Fatalf("meta = %#v, want 3 of 10 matches", meta)
	}
}

func TestInvalidPatternIsReportedNotPanicked(t *testing.T) {
	store := seedRun(t, "r-bad", []string{"[t/u] anything"})

	_, _, err := store.ReadFiltered("r-bad", "t", "u", Filter{Pattern: "([unclosed"})
	if err == nil {
		t.Fatal("an invalid pattern returned no error")
	}
	if !errors.Is(err, ErrInvalidPattern) {
		t.Fatalf("err = %v, want ErrInvalidPattern so the transport can map it to a 400", err)
	}
}

func TestPatternOverLengthIsRejectedBeforeCompiling(t *testing.T) {
	store := seedRun(t, "r-long", []string{"[t/u] x"})

	_, _, err := store.ReadFiltered("r-long", "t", "u", Filter{Pattern: strings.Repeat("a", maxPatternBytes+1)})
	if !errors.Is(err, ErrInvalidPattern) {
		t.Fatalf("err = %v, want ErrInvalidPattern", err)
	}
}

func TestContextBeyondTheCeilingIsRejected(t *testing.T) {
	store := seedRun(t, "r-ctxmax", []string{"[t/u] x"})

	_, _, err := store.ReadFiltered("r-ctxmax", "t", "u", Filter{Pattern: "x", Context: maxContextLines + 1})
	if !errors.Is(err, ErrInvalidPattern) {
		t.Fatalf("err = %v, want the context ceiling enforced", err)
	}
}

func FuzzFilter(f *testing.F) {
	f.Add("FAIL", 1, 0, 0)
	f.Add("^go", 0, 2, 3)
	f.Add("(", 0, 0, 0)
	f.Fuzz(func(t *testing.T, pattern string, context, offset, limit int) {
		if context < 0 || context > maxContextLines || offset < 0 || limit < 0 {
			t.Skip()
		}
		store := seedRun(t, "r-fuzz", []string{
			"[t/u] alpha", "[t/u] FAIL beta", "[t/u] gamma", "[t/u] FAIL delta",
		})
		_, _, _ = store.ReadFiltered("r-fuzz", "t", "u",
			Filter{Pattern: pattern, Context: context, Offset: offset, Limit: limit})
	})
}

func TestAPatternCannotRecoverASecretTheWriterRedacted(t *testing.T) {
	const secret = "s3cr3t-token-value"
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	file, err := store.Create("r-redact")
	if err != nil {
		t.Fatal(err)
	}
	writer := redact.NewWriter(file, [][]byte{[]byte(secret)})
	if _, err := writer.Write([]byte("[t/u] using " + secret + " now\n[t/u] done\n")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.BuildIndex("r-redact"); err != nil {
		t.Fatal(err)
	}

	body, meta, err := store.ReadFiltered("r-redact", "t", "u", Filter{Pattern: regexp.QuoteMeta(secret)})
	if err != nil {
		t.Fatal(err)
	}
	if meta.MatchedLines != 0 || len(body) != 0 {
		t.Fatalf("a pattern recovered redacted content: body = %q meta = %#v", body, meta)
	}

	all, _, err := store.ReadFiltered("r-redact", "t", "u", Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(all), secret) {
		t.Fatalf("the secret reached disk unredacted: %q", all)
	}
}

func TestAPathologicalPatternOverALargeStepStaysBounded(t *testing.T) {
	lines := make([]string, 0, 60000)
	for i := 0; i < 60000; i++ {
		lines = append(lines, "[t/u] "+strings.Repeat("a", 40))
	}
	store := seedRun(t, "r-slow", lines)

	started := time.Now()
	_, meta, err := store.ReadFiltered("r-slow", "t", "u", Filter{Pattern: "(a+)+b", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed > 10*time.Second {
		t.Fatalf("a catastrophic-looking pattern took %s; RE2 should not backtrack", elapsed)
	}
	if meta.MatchedLines != 0 {
		t.Fatalf("meta = %#v, want no matches", meta)
	}
}

func seedOversized(t *testing.T, runID string) *Store {
	t.Helper()
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	file, err := store.Create(runID)
	if err != nil {
		t.Fatal(err)
	}
	writer := bufio.NewWriter(file)
	line := "[t/u] " + strings.Repeat("z", 200) + "\n"
	for written := 0; written < maxReadBytes+(1<<16); written += len(line) {
		if _, err := writer.WriteString(line); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.BuildIndex(runID); err != nil {
		t.Fatal(err)
	}
	return store
}

func TestAnOversizedStepIsReadableInsteadOfFailing(t *testing.T) {
	store := seedOversized(t, "r-big")

	body, meta, err := store.ReadFiltered("r-big", "t", "u", Filter{})
	if err != nil {
		t.Fatalf("an oversized step is still unreadable: %v", err)
	}
	if len(body) == 0 {
		t.Fatal("returned no bytes for an oversized step")
	}
	if !meta.Truncated {
		t.Fatalf("meta = %#v, want Truncated so the caller knows bytes were withheld", meta)
	}
	if meta.Bytes <= maxReadBytes {
		t.Fatalf("meta.Bytes = %d, want the true step size above the ceiling", meta.Bytes)
	}
	if int64(len(body)) > maxReadBytes {
		t.Fatalf("returned %d bytes, above the %d ceiling", len(body), maxReadBytes)
	}
}

func TestTailOfAnOversizedStepReturnsTheEndNotTheStart(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	file, err := store.Create("r-bigtail")
	if err != nil {
		t.Fatal(err)
	}
	writer := bufio.NewWriter(file)
	filler := "[t/u] " + strings.Repeat("z", 200) + "\n"
	for written := 0; written < maxReadBytes+(1<<16); written += len(filler) {
		if _, err := writer.WriteString(filler); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := writer.WriteString("[t/u] THE-VERY-LAST-LINE\n"); err != nil {
		t.Fatal(err)
	}
	if err := writer.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.BuildIndex("r-bigtail"); err != nil {
		t.Fatal(err)
	}

	body, meta, err := store.ReadFiltered("r-bigtail", "t", "u", Filter{Tail: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "THE-VERY-LAST-LINE") {
		t.Fatalf("tail of an oversized step returned the head; meta = %#v", meta)
	}
}

func TestUnfilteredReadKeepsRefusingAnOversizedStep(t *testing.T) {
	store := seedOversized(t, "r-legacy")

	if _, err := store.Read("r-legacy", "t", "u"); !errors.Is(err, ErrLogSliceTooLarge) {
		t.Fatalf("Read error = %v, want ErrLogSliceTooLarge; a caller that cannot see Meta must not receive a silently short log", err)
	}
}
