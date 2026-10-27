package gitcache

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
)

var errArchiveAttrGap = errors.New("archive export attributes are not supported")

// Fail closed on export rules until Git's complete pattern/macro/pretty-format
// semantics have parity coverage. A gap is NOT a completed clean comparison or
// a decoder mismatch. Detection is conservative (overridden/unused rules can
// also exclude a sample), so an attribute-bearing repo cannot imply a green soak.
func checkGoArchiveAttributes(ctx context.Context, r *git.Repository, tree *object.Tree) error {
	if store, ok := r.Storer.(*atomicReferenceStore); ok {
		if _, err := os.Lstat(filepath.Join(store.dir, "info", "attributes")); err == nil {
			return errArchiveAttrGap
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	config, err := r.Config()
	if err != nil {
		return err
	}
	for _, section := range config.Raw.Sections {
		if strings.EqualFold(section.Name, "core") {
			for _, option := range section.Options {
				if strings.EqualFold(option.Key, "attributesFile") && option.Value != "" {
					return errArchiveAttrGap
				}
			}
		}
	}
	return walkGoTree(ctx, r, tree, func(name string, entry object.TreeEntry) error {
		if path.Base(name) != ".gitattributes" || (entry.Mode != filemode.Regular && entry.Mode != filemode.Executable && entry.Mode != filemode.Deprecated) {
			return nil
		}
		blob, err := r.BlobObject(entry.Hash)
		if err != nil {
			return err
		}
		// Bound exceptional metadata independently of the archive's streamed files.
		if blob.Size > 1<<20 {
			return errArchiveAttrGap
		}
		reader, err := blob.Reader()
		if err != nil {
			return err
		}

		found, parseErr := scanArchiveExportRules(contextReader{ctx, reader})
		// Drain even after a parse error so the decoded-object hash guard still runs.
		_, readErr := io.Copy(io.Discard, contextReader{ctx, reader})
		closeErr := reader.Close()
		if readErr != nil || closeErr != nil {
			return errors.Join(readErr, closeErr)
		}
		if parseErr != nil {
			return errors.Join(errArchiveAttrGap, parseErr)
		}
		if found {
			return errArchiveAttrGap
		}

		return nil
	})
}

// This is only a conservative gap detector, not a Git pattern/macro matcher.
// Skip the (possibly C-quoted) pattern; inspect attribute tokens, including
// macro definitions. Never let an untrusted malformed line panic the server.
func scanArchiveExportRules(input io.Reader) (bool, error) {
	scanner := bufio.NewScanner(input)
	found := false
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if line[0] == '"' {
			end := -1
			for i := 1; i < len(line); i++ {
				if line[i] == '\\' {
					i++
					continue
				}
				if line[i] == '"' {
					end = i
					break
				}
			}
			if end < 0 {
				return false, errArchiveAttrGap
			}
			line = line[end+1:]
		} else {
			// Keep only tokens after the first unquoted pattern token.
			fields := strings.Fields(scanner.Text())
			if len(fields) < 2 {
				continue
			}
			line = strings.Join(fields[1:], " ")
		}
		for _, attr := range strings.Fields(line) {
			name, _, _ := strings.Cut(attr, "=")
			if name == "export-ignore" || name == "export-subst" {
				found = true
			}
		}
	}
	return found, scanner.Err()
}
