// Package holdfs applies agent patches to the source tree of a held assist
// attempt. The server parses and applies a unified diff against the files it
// read from the Pod, and the Pod-side helper (`oberth internal-hold-fs`)
// reads and writes those files under one root with no shell, no symlink
// following and bounded sizes. See docs/assisted-continuation.md.
package holdfs

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"path"
	"strconv"
	"strings"
)

// Limits bound one patch.
type Limits struct {
	MaxBytes int64
	MaxFiles int
}

// FilePatch is the parsed unified diff of one file.
type FilePatch struct {
	// Path is the cleaned, root-relative path (the b/ side, or the a/ side
	// for a deletion).
	Path string
	// Create and Delete mark /dev/null on the old or new side.
	Create, Delete bool
	Hunks          []Hunk
}

// Hunk is one @@ block.
type Hunk struct {
	OldStart, OldLines, NewStart, NewLines int
	Lines                                  []Line
}

// Line is one hunk line: ' ' context, '-' removed, '+' added.
type Line struct {
	Kind byte
	Text string
	// NoNewline records "\ No newline at end of file" after this line.
	NoNewline bool
}

var (
	ErrLimit   = errors.New("holdfs: patch exceeds limit")
	ErrPath    = errors.New("holdfs: patch path refused")
	ErrFormat  = errors.New("holdfs: unified diff format")
	ErrContext = errors.New("holdfs: hunk does not apply")
)

// Parse reads a unified diff. Only plain text file patches are accepted:
// no renames, copies, mode changes or binary patches, one patch per file,
// every path relative, cleaned, outside .git and free of parent references.
func Parse(diff []byte, limits Limits) ([]FilePatch, error) {
	if limits.MaxBytes > 0 && int64(len(diff)) > limits.MaxBytes {
		return nil, fmt.Errorf("%w: %d bytes, at most %d", ErrLimit, len(diff), limits.MaxBytes)
	}
	if !bytes.HasSuffix(diff, []byte("\n")) {
		diff = append(append([]byte{}, diff...), '\n')
	}
	scanner := bufio.NewScanner(bytes.NewReader(diff))
	scanner.Buffer(make([]byte, 0, 64<<10), 4<<20)
	var patches []FilePatch
	seen := map[string]bool{}
	var current *FilePatch
	var hunk *Hunk
	lineNumber := 0
	oldRemaining, newRemaining := 0, 0
	finishHunk := func() error {
		if hunk == nil {
			return nil
		}
		if oldRemaining != 0 || newRemaining != 0 {
			return fmt.Errorf("%w: hunk at line %d has wrong line counts", ErrFormat, lineNumber)
		}
		current.Hunks = append(current.Hunks, *hunk)
		hunk = nil
		return nil
	}
	finishFile := func() error {
		if err := finishHunk(); err != nil {
			return err
		}
		if current != nil {
			if len(current.Hunks) == 0 && !current.Delete && !current.Create {
				return fmt.Errorf("%w: %s has no hunks", ErrFormat, current.Path)
			}
			patches = append(patches, *current)
			current = nil
		}
		return nil
	}
	for scanner.Scan() {
		lineNumber++
		text := scanner.Text()
		switch {
		case hunk != nil && (oldRemaining > 0 || newRemaining > 0):
			if text == "" {
				text = " "
			}
			kind, body := text[0], text[1:]
			switch kind {
			case ' ':
				oldRemaining--
				newRemaining--
			case '-':
				oldRemaining--
			case '+':
				newRemaining--
			case '\\':
				if len(hunk.Lines) == 0 {
					return nil, fmt.Errorf("%w: line %d: stray newline marker", ErrFormat, lineNumber)
				}
				hunk.Lines[len(hunk.Lines)-1].NoNewline = true
				continue
			default:
				return nil, fmt.Errorf("%w: line %d: unexpected %q inside hunk", ErrFormat, lineNumber, text)
			}
			if oldRemaining < 0 || newRemaining < 0 {
				return nil, fmt.Errorf("%w: line %d: hunk overruns its counts", ErrFormat, lineNumber)
			}
			hunk.Lines = append(hunk.Lines, Line{Kind: kind, Text: body})
		case strings.HasPrefix(text, "\\ No newline"):
			if hunk == nil || len(hunk.Lines) == 0 {
				return nil, fmt.Errorf("%w: line %d: stray newline marker", ErrFormat, lineNumber)
			}
			hunk.Lines[len(hunk.Lines)-1].NoNewline = true
		case strings.HasPrefix(text, "--- "):
			if err := finishFile(); err != nil {
				return nil, err
			}
			if limits.MaxFiles > 0 && len(patches) >= limits.MaxFiles {
				return nil, fmt.Errorf("%w: more than %d files", ErrLimit, limits.MaxFiles)
			}
			oldPath, err := headerPath(text[4:])
			if err != nil {
				return nil, fmt.Errorf("line %d: %w", lineNumber, err)
			}
			if !scanner.Scan() {
				return nil, fmt.Errorf("%w: line %d: missing +++ header", ErrFormat, lineNumber)
			}
			lineNumber++
			next := scanner.Text()
			if !strings.HasPrefix(next, "+++ ") {
				return nil, fmt.Errorf("%w: line %d: expected +++ header", ErrFormat, lineNumber)
			}
			newPath, err := headerPath(next[4:])
			if err != nil {
				return nil, fmt.Errorf("line %d: %w", lineNumber, err)
			}
			patch := FilePatch{Create: oldPath == "", Delete: newPath == ""}
			switch {
			case patch.Create && patch.Delete:
				return nil, fmt.Errorf("%w: line %d: both sides are /dev/null", ErrFormat, lineNumber)
			case patch.Delete:
				patch.Path = oldPath
			case patch.Create:
				patch.Path = newPath
			case oldPath != newPath:
				return nil, fmt.Errorf("%w: line %d: renames are not applied (%s -> %s)", ErrPath, lineNumber, oldPath, newPath)
			default:
				patch.Path = newPath
			}
			if seen[patch.Path] {
				return nil, fmt.Errorf("%w: line %d: %s patched twice", ErrFormat, lineNumber, patch.Path)
			}
			seen[patch.Path] = true
			current = &patch
		case strings.HasPrefix(text, "@@ "):
			if current == nil {
				return nil, fmt.Errorf("%w: line %d: hunk before file header", ErrFormat, lineNumber)
			}
			if err := finishHunk(); err != nil {
				return nil, err
			}
			parsed, err := parseHunkHeader(text)
			if err != nil {
				return nil, fmt.Errorf("line %d: %w", lineNumber, err)
			}
			hunk = &parsed
			oldRemaining, newRemaining = parsed.OldLines, parsed.NewLines
		case strings.HasPrefix(text, "diff ") || strings.HasPrefix(text, "index ") || text == "" || strings.HasPrefix(text, "similarity ") || strings.HasPrefix(text, "rename ") || strings.HasPrefix(text, "copy ") || strings.HasPrefix(text, "old mode") || strings.HasPrefix(text, "new mode") || strings.HasPrefix(text, "deleted file mode") || strings.HasPrefix(text, "new file mode"):
			if strings.HasPrefix(text, "rename ") || strings.HasPrefix(text, "copy ") || strings.HasPrefix(text, "similarity ") {
				return nil, fmt.Errorf("%w: line %d: renames and copies are not applied", ErrPath, lineNumber)
			}
			if strings.HasPrefix(text, "old mode") || strings.HasPrefix(text, "new mode") {
				return nil, fmt.Errorf("%w: line %d: mode changes are not applied", ErrFormat, lineNumber)
			}
		case strings.HasPrefix(text, "Binary files") || strings.HasPrefix(text, "GIT binary patch"):
			return nil, fmt.Errorf("%w: line %d: binary patches are not applied", ErrFormat, lineNumber)
		default:
			if current == nil {
				// Commit message or other preamble before the first header.
				continue
			}
			return nil, fmt.Errorf("%w: line %d: unexpected %q", ErrFormat, lineNumber, text)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrFormat, err)
	}
	if err := finishFile(); err != nil {
		return nil, err
	}
	if len(patches) == 0 {
		return nil, fmt.Errorf("%w: no file patches", ErrFormat)
	}
	return patches, nil
}

// headerPath cleans a ---/+++ header path: strips a/ or b/, a trailing
// tab-separated timestamp, and refuses anything that could leave the root.
// /dev/null returns "".
func headerPath(raw string) (string, error) {
	if index := strings.IndexByte(raw, '\t'); index >= 0 {
		raw = raw[:index]
	}
	raw = strings.TrimSpace(raw)
	if raw == "/dev/null" {
		return "", nil
	}
	if strings.HasPrefix(raw, "a/") || strings.HasPrefix(raw, "b/") {
		raw = raw[2:]
	}
	return CleanPath(raw)
}

// CleanPath validates a root-relative path for the held source tree: no
// empty, absolute, parent, NUL or control characters, nothing under .git,
// and the cleaned spelling must equal the given one.
func CleanPath(raw string) (string, error) {
	if raw == "" || raw == "." {
		return "", fmt.Errorf("%w: empty path", ErrPath)
	}
	if len(raw) > 1024 {
		return "", fmt.Errorf("%w: path longer than 1024 bytes", ErrPath)
	}
	if strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, "\\") {
		return "", fmt.Errorf("%w: %q is absolute", ErrPath, raw)
	}
	for _, character := range raw {
		if character < 0x20 || character == 0x7f {
			return "", fmt.Errorf("%w: %q contains control characters", ErrPath, raw)
		}
	}
	cleaned := path.Clean(raw)
	if cleaned != raw || cleaned == ".." || strings.HasPrefix(cleaned, "../") || strings.Contains(cleaned, "/../") {
		return "", fmt.Errorf("%w: %q must be a clean relative path", ErrPath, raw)
	}
	for _, segment := range strings.Split(cleaned, "/") {
		if segment == ".git" || segment == "" {
			return "", fmt.Errorf("%w: %q touches a Git directory", ErrPath, raw)
		}
	}
	return cleaned, nil
}

func parseHunkHeader(text string) (Hunk, error) {
	// @@ -l[,s] +l[,s] @@[ section]
	rest := strings.TrimPrefix(text, "@@ ")
	end := strings.Index(rest, " @@")
	if end < 0 {
		return Hunk{}, fmt.Errorf("%w: malformed hunk header %q", ErrFormat, text)
	}
	fields := strings.Fields(rest[:end])
	if len(fields) != 2 || !strings.HasPrefix(fields[0], "-") || !strings.HasPrefix(fields[1], "+") {
		return Hunk{}, fmt.Errorf("%w: malformed hunk header %q", ErrFormat, text)
	}
	var hunk Hunk
	var err error
	if hunk.OldStart, hunk.OldLines, err = parseRange(fields[0][1:]); err != nil {
		return Hunk{}, fmt.Errorf("%w: %q: %w", ErrFormat, text, err)
	}
	if hunk.NewStart, hunk.NewLines, err = parseRange(fields[1][1:]); err != nil {
		return Hunk{}, fmt.Errorf("%w: %q: %w", ErrFormat, text, err)
	}
	return hunk, nil
}

func parseRange(value string) (start, lines int, err error) {
	lines = 1
	if index := strings.IndexByte(value, ','); index >= 0 {
		if lines, err = strconv.Atoi(value[index+1:]); err != nil {
			return 0, 0, err
		}
		value = value[:index]
	}
	if start, err = strconv.Atoi(value); err != nil {
		return 0, 0, err
	}
	if start < 0 || lines < 0 || start > 1<<24 || lines > 1<<24 {
		return 0, 0, errors.New("range out of bounds")
	}
	return start, lines, nil
}

// Apply applies one file's hunks to its current content with exact context
// matching: no fuzz, no offset search. A creation requires no current file;
// a deletion requires the whole file to match its removed lines.
func Apply(current []byte, exists bool, patch FilePatch) ([]byte, error) {
	if patch.Create && exists {
		return nil, fmt.Errorf("%w: %s already exists", ErrContext, patch.Path)
	}
	if !patch.Create && !exists {
		return nil, fmt.Errorf("%w: %s does not exist", ErrContext, patch.Path)
	}
	oldLines, trailingNewline := splitLines(current)
	var out []string
	cursor := 0 // index into oldLines
	outTrailingNewline := trailingNewline
	for index, hunk := range patch.Hunks {
		start := hunk.OldStart - 1
		if hunk.OldLines == 0 {
			start = hunk.OldStart // insertion after line OldStart
		}
		if start < cursor || start > len(oldLines) {
			return nil, fmt.Errorf("%w: %s hunk %d starts outside the file", ErrContext, patch.Path, index+1)
		}
		out = append(out, oldLines[cursor:start]...)
		cursor = start
		for _, line := range hunk.Lines {
			switch line.Kind {
			case ' ', '-':
				if cursor >= len(oldLines) || oldLines[cursor] != line.Text {
					return nil, fmt.Errorf("%w: %s hunk %d context mismatch at line %d", ErrContext, patch.Path, index+1, cursor+1)
				}
				if line.Kind == ' ' {
					out = append(out, line.Text)
				}
				cursor++
			case '+':
				out = append(out, line.Text)
			}
			if line.NoNewline && line.Kind != '-' {
				outTrailingNewline = false
			}
			if line.NoNewline && line.Kind == '-' && !trailingNewline {
				// The old file ended without a newline here; a replacement
				// line decides the new ending.
				outTrailingNewline = true
			}
		}
	}
	out = append(out, oldLines[cursor:]...)
	if patch.Delete {
		if len(out) != 0 {
			return nil, fmt.Errorf("%w: %s deletion leaves content", ErrContext, patch.Path)
		}
		return nil, nil
	}
	if len(out) == 0 {
		return []byte{}, nil
	}
	result := strings.Join(out, "\n")
	if outTrailingNewline {
		result += "\n"
	}
	return []byte(result), nil
}

func splitLines(content []byte) ([]string, bool) {
	if len(content) == 0 {
		return nil, true
	}
	text := string(content)
	trailing := strings.HasSuffix(text, "\n")
	text = strings.TrimSuffix(text, "\n")
	return strings.Split(text, "\n"), trailing
}
