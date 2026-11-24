// Package setuptui presents three plain installation prompts and engine progress.
//
// Spec: docs/plans/c9-install-and-upgrade.md#owned-data-and-public-contract
package setuptui

import (
	"bufio"
	"context"
	"fmt"
	"golang.org/x/text/width"
	"io"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/oberthci/oberth/internal/installer"
)

func containsContext(contexts []string, selected string) bool {
	for _, name := range contexts {
		if selected == name {
			return true
		}
	}
	return false
}

// Ask selects a context and public address using one caller-owned scanner.
// Terminal input cancellation is owned by the CLI, which closes its input on exit.
func Ask(ctx context.Context, in *bufio.Scanner, out io.Writer, contexts []string) (installer.Choices, error) {
	var c installer.Choices
	contextPrompt := "Context (choose one: " + strings.Join(contexts, ", ") + "): "
	if len(contexts) == 1 {
		contextPrompt = "Context [" + contexts[0] + "]: "
	}
	for _, prompt := range []struct {
		text   string
		target *string
	}{
		{contextPrompt, &c.Context},
		{"Server URL (blank uses the API host): ", &c.URL},
	} {
		if err := ctx.Err(); err != nil {
			return c, err
		}
		if err := writeLine(out, strings.TrimSpace(prompt.text)); err != nil {
			return c, err
		}
		if !in.Scan() {
			if err := in.Err(); err != nil {
				return c, err
			}
			return c, installer.NewProblem("terminal", nil, "")
		}
		*prompt.target = strings.TrimSpace(in.Text())
		if prompt.target == &c.Context {
			if c.Context == "" && len(contexts) == 1 {
				c.Context = contexts[0]
			}
			if !containsContext(contexts, c.Context) {
				return c, installer.NewProblem("context-missing", nil, "Context: "+c.Context)
			}
		}
	}
	return c, nil
}

// Terminal is where a review is read and answered, and its size. Zero Rows
// means the input is not a terminal: the review is printed whole before the
// approval question.
type Terminal struct {
	In            *bufio.Scanner
	Out           io.Writer
	Rows, Columns int
}

// Review shows the plan a terminal page at a time, preserves copyable values,
// asks for approval after the last page and returns the approved digest.
func Review(ctx context.Context, t Terminal, p *installer.Plan) (string, error) {
	approved, err := approve(ctx, t, p.Review())
	if err != nil || !approved {
		return "", err
	}
	return p.Digest(), nil
}

func approve(ctx context.Context, t Terminal, review []string) (bool, error) {
	var rendered strings.Builder
	for _, line := range review {
		if err := writeLine(&rendered, terminalText(line)); err != nil {
			return false, err
		}
	}
	pages := paginate(strings.Split(strings.TrimSuffix(rendered.String(), "\n"), "\n"), t.Rows, t.Columns)
	for i, page := range pages {
		if _, err := io.WriteString(t.Out, page); err != nil {
			return false, err
		}
		if i == len(pages)-1 {
			break
		}
		if _, err := fmt.Fprintf(t.Out, pagePrompt, i+1, len(pages)); err != nil {
			return false, err
		}
		if _, err := answer(ctx, t.In); err != nil {
			return false, err
		}
	}
	if _, err := fmt.Fprint(t.Out, approvalPrompt); err != nil {
		return false, err
	}
	reply, err := answer(ctx, t.In)
	if err != nil || strings.EqualFold(reply, "y") {
		return err == nil, err
	}
	_, err = fmt.Fprintln(t.Out, "\nNot applied; nothing was changed.")
	return false, err
}

// answer reads one line from the person at the terminal.
func answer(ctx context.Context, in *bufio.Scanner) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if !in.Scan() {
		if err := in.Err(); err != nil {
			return "", err
		}
		return "", installer.NewProblem("terminal", nil, "")
	}
	return strings.TrimSpace(in.Text()), nil
}

const (
	pagePrompt     = "Page %d of %d; press Enter for the next page."
	approvalPrompt = "Apply? [y/N] "
)

// paginate fills each page up to the rows above its prompt, counting the
// prompt's own wrapping on a narrow terminal. A line is never split; one
// longer than a page has a page of its own.
func paginate(lines []string, rows, columns int) []string {
	if rows <= 0 {
		return []string{strings.Join(lines, "\n") + "\n"}
	}
	if columns <= 0 {
		columns = 80
	}
	// The longest page prompt this review can print: its page count is at most its line count.
	prompt := max(rowCount(fmt.Sprintf(pagePrompt, len(lines), len(lines)), columns), rowCount(approvalPrompt, columns))
	capacity := max(rows-prompt, 1)
	var pages []string
	var page strings.Builder
	used := 0
	for _, line := range lines {
		height := rowCount(line, columns)
		if used > 0 && used+height > capacity {
			pages, used = append(pages, page.String()), 0
			page.Reset()
		}
		page.WriteString(line + "\n")
		used += height
	}
	return append(pages, page.String())
}

func rowCount(line string, columns int) int {
	rows := 0
	for remaining := line; remaining != ""; rows++ {
		remaining = remaining[lineBreak(remaining, columns):]
	}
	return max(rows, 1)
}

// Progress renders engine reasons without truncation or private values.
func Progress(out io.Writer) installer.Progress {
	return func(phase installer.Phase) error { _, err := fmt.Fprintln(out, phase); return err }
}
func writeLine(out io.Writer, line string) error {
	if lines := CopyableLines(line); lines != nil {
		for _, value := range lines {
			if _, err := fmt.Fprintln(out, value); err != nil {
				return err
			}
		}
		return nil
	}
	_, err := fmt.Fprintln(out, line)
	return err
}

// lineBreak returns where a terminal of columns cells wraps line; it always
// advances, even past one character wider than the terminal.
func lineBreak(line string, columns int) int {
	cells, boundary := 0, 0
	for i, r := range line {
		if cells+cellWidth(r) > columns {
			if boundary > 0 {
				return boundary
			}
			if i == 0 {
				_, size := utf8.DecodeRuneInString(line)
				return size
			}
			return i
		}
		cells += cellWidth(r)
		if r == ' ' || r == ',' {
			_, size := utf8.DecodeRuneInString(line[i:])
			boundary = i + size
		}
	}
	return len(line)
}
func cellWidth(r rune) int {
	if r == '\t' {
		return 8
	}
	if unicode.Is(unicode.Mn, r) || unicode.Is(unicode.Me, r) || unicode.Is(unicode.Cf, r) {
		return 0
	}
	switch width.LookupRune(r).Kind() {
	case width.EastAsianWide, width.EastAsianFullwidth:
		return 2
	}
	return 1
}

func terminalText(line string) string {
	quoted := strconv.QuoteToGraphic(line)
	return quoted[1 : len(quoted)-1]
}
