package setuptui

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/installer"
)

func reviewLines(n int) []string {
	lines := make([]string, n)
	for i := range lines {
		lines[i] = fmt.Sprintf("Review line %02d", i+1)
	}
	return lines
}

func terminal(input string, out *bytes.Buffer, rows, columns int) Terminal {
	return Terminal{In: bufio.NewScanner(strings.NewReader(input)), Out: out, Rows: rows, Columns: columns}
}

const pagePromptEnd = "; press Enter for the next page."

// pageRows counts the rows printed above each page prompt. The prompt ends
// without a newline, as the terminal echoes the Enter that answers it.
func pageRows(output string) []int {
	var pages []int
	rows := 0
	for _, line := range strings.Split(output, "\n") {
		if _, next, prompt := strings.Cut(line, pagePromptEnd); prompt {
			pages = append(pages, rows)
			rows = 0
			line = next
		}
		if line != "" {
			rows++
		}
	}
	return pages
}

// checkWholeReview requires every line once and the question after the last.
func checkWholeReview(t *testing.T, text string, lines []string) {
	t.Helper()
	for _, line := range lines {
		if strings.Count(text, line+"\n") != 1 || strings.Count(text, line) != 1 {
			t.Fatal("review line not shown exactly once", line, text)
		}
	}
	if strings.Index(text, "Apply? [y/N] ") < strings.Index(text, lines[len(lines)-1]) || !strings.HasSuffix(text, "Apply? [y/N] ") {
		t.Fatal("approval asked before the last page", text)
	}
}

// The review is never refused for its length: a terminal of 24 rows reads a
// 30-row review a page at a time and is asked for approval after the last
// page; a terminal of 50 rows and a non-terminal input read it whole.
func TestReviewPagesOnTheTerminalHeight(t *testing.T) {
	lines := reviewLines(30)
	for name, tc := range map[string]struct {
		rows  int
		input string
		pages int
	}{
		"24-row terminal": {24, "\ny\n", 2},
		"50-row terminal": {50, "y\n", 1},
		"not a terminal":  {0, "y\n", 1},
	} {
		t.Run(name, func(t *testing.T) {
			var out bytes.Buffer
			approved, err := approve(context.Background(), terminal(tc.input, &out, tc.rows, 80), lines)
			if err != nil || !approved {
				t.Fatal("review not approved", err, out.String())
			}
			checkWholeReview(t, out.String(), lines)
			prompts := pageRows(out.String())
			if len(prompts)+1 != tc.pages {
				t.Fatal("wrong page count", len(prompts)+1, out.String())
			}
			for _, rows := range prompts {
				if rows != tc.rows-1 {
					t.Fatal("a page does not fill the terminal above its prompt", rows, out.String())
				}
			}
		})
	}
}

func TestReviewPagingStopsWithTheTerminal(t *testing.T) {
	var out bytes.Buffer
	_, err := approve(context.Background(), terminal("", &out, 24, 80), reviewLines(30))
	var problem *installer.Problem
	if !errors.As(err, &problem) || problem.Code != "terminal" || strings.Contains(out.String(), "Apply?") {
		t.Fatal("closed input approved or asked", err, out.String())
	}
	out.Reset()
	approved, err := approve(context.Background(), terminal("\nn\n", &out, 24, 80), reviewLines(30))
	if err != nil || approved || !strings.HasSuffix(out.String(), "Not applied; nothing was changed.\n") {
		t.Fatal("declined review applied", approved, err, out.String())
	}
}

// On a terminal narrower than the page prompt, the prompt's own rows are kept
// free, so the first line of a page does not scroll off: 30 columns wrap the
// prompt to 2 rows, leaving 4 of 6 for the review.
func TestReviewPageKeepsRoomForAWrappedPrompt(t *testing.T) {
	pages := paginate(reviewLines(10), 6, 30)
	if len(pages) != 3 {
		t.Fatalf("wrong pages %q", pages)
	}
	for i, page := range pages[:2] {
		if rows := strings.Count(page, "\n"); rows+rowCount(fmt.Sprintf(pagePrompt, i+1, len(pages)), 30) > 6 {
			t.Fatal("page and prompt exceed the terminal", i+1, rows)
		}
	}
}

// Rows follow the terminal's width: a line wider than the screen takes the
// rows it wraps to, and a page never splits a line.
func TestReviewPageCountsWrappedRows(t *testing.T) {
	long := strings.Repeat("word ", 40)
	pages := paginate([]string{long, "a", "b", "c"}, 4, 50)
	if len(pages) != 2 || pages[0] != long+"\n" || pages[1] != "a\nb\nc\n" {
		t.Fatalf("wrong pages %q", pages)
	}
}
