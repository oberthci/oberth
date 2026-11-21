package argoworkflow

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

func documentationPages(t *testing.T) []string {
	t.Helper()
	pages, err := filepath.Glob("../../docs/*.md")
	if err != nil || len(pages) < 6 {
		t.Fatalf("documentation pages missing: %v", err)
	}
	return append([]string{"../../README.md"}, pages...)
}

func TestDocumentationRelativeLinks(t *testing.T) {
	t.Parallel()
	for _, file := range documentationPages(t) {
		body, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for _, destination := range documentationLinks(string(body)) {
			if err := documentationLink(file, destination); err != nil {
				t.Errorf("%s: %s: %v", file, destination, err)
			}
		}
	}
}

// Cover inline links, reference definitions, and HTML link/image attributes.
func documentationLinks(body string) []string {
	patterns := []string{
		`\[[^\]]*\]\(\s*(<[^>]*>|[^\s)]*)[^)\n]*\)`,
		`(?m)^ {0,3}\[[^\]\n]+\]:\s*(<[^>]*>|\S+)`,
		`(?i)(?:href|src)\s*=\s*["']([^"']*)["']`,
	}
	var links []string
	for _, pattern := range patterns {
		for _, match := range regexp.MustCompile(pattern).FindAllStringSubmatch(body, -1) {
			links = append(links, strings.Trim(match[1], "<>"))
		}
	}
	return links
}

func documentationLink(file, destination string) error {
	u, err := url.Parse(destination)
	if err != nil {
		return err
	}
	if u.IsAbs() || u.Host != "" {
		return nil
	}
	target := file
	if u.Path != "" {
		target = filepath.Join(filepath.Dir(file), filepath.FromSlash(u.Path))
	}
	info, err := os.Stat(target)
	if err != nil {
		return err
	}
	if info.IsDir() {
		return fmt.Errorf("link must name a file")
	}
	if u.Fragment == "" {
		return nil
	}
	body, err := os.ReadFile(target)
	if err != nil {
		return err
	}
	if !documentationAnchors(string(body))[u.Fragment] {
		return fmt.Errorf("missing anchor %q", u.Fragment)
	}
	return nil
}

func documentationAnchors(body string) map[string]bool {
	anchors := map[string]bool{}
	fenced := false
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			fenced = !fenced
		}
		if fenced || !strings.HasPrefix(line, "#") {
			continue
		}
		heading := strings.TrimSpace(strings.Trim(line, "#"))
		slug := strings.Map(func(r rune) rune {
			switch {
			case r == ' ':
				return '-'
			case unicode.IsLetter(r), unicode.IsNumber(r), r == '-', r == '_':
				return unicode.ToLower(r)
			default:
				return -1
			}
		}, heading)
		anchors[slug] = true
	}
	return anchors
}

func TestDocumentationLinksRefuseMissingTargets(t *testing.T) {
	t.Parallel()
	file := "../../README.md"
	for _, destination := range []string{"docs/absent.md", "#absent", "docs/install.md#absent"} {
		if err := documentationLink(file, destination); err == nil {
			t.Errorf("accepted missing target %s", destination)
		}
	}
	links := documentationLinks(`[page](docs/install.md) ![image](picture.png)
[guide]: docs/agents.md
<a href="docs/security.md">security</a>`)
	if len(links) != 4 {
		t.Fatalf("not every supported link found: %v", links)
	}
}

func TestDocumentationSourceBudgets(t *testing.T) {
	t.Parallel()
	pages := documentationPages(t)
	examples, err := filepath.Glob("../../docs/examples/*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	lines := 0
	for _, file := range append(pages, examples...) {
		body, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		count, err := documentationLines(string(body))
		if err != nil {
			t.Errorf("%s: %v", file, err)
		}
		lines += count
	}
	if lines > 1300 {
		t.Errorf("documentation has %d lines; stop above 1300", lines)
	}
}

func documentationLines(body string) (int, error) {
	rows := strings.Split(strings.TrimSuffix(body, "\n"), "\n")
	for number, line := range rows {
		if utf8.RuneCountInString(line) > 100 {
			return len(rows), fmt.Errorf("line %d exceeds 100 characters", number+1)
		}
	}
	return len(rows), nil
}

func TestDocumentationLineLength(t *testing.T) {
	t.Parallel()
	if _, err := documentationLines(strings.Repeat("x", 100)); err != nil {
		t.Fatal("100-character line refused", err)
	}
	if _, err := documentationLines(strings.Repeat("x", 101)); err == nil {
		t.Fatal("101-character line accepted")
	}
}

func TestDocumentationWrappedLinks(t *testing.T) {
	t.Parallel()
	body := "[text that wraps\nonto another line](docs/absent.md)"
	links := documentationLinks(body)
	if len(links) != 1 || links[0] != "docs/absent.md" {
		t.Fatalf("wrapped relative link not found: %v", links)
	}
	if err := documentationLink("../../README.md", links[0]); err == nil {
		t.Fatal("wrapped link to missing file accepted")
	}
}
