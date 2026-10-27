// Package archivecheck compares the server's archive path with Git without
// fetching, repacking, or modifying the supplied bare repository.
package archivecheck

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/oberthci/oberth/internal/gitcache"
)

// Difference identifies the first different entry or byte (zero based).
type Difference struct {
	Path   string
	Offset int64
	Detail string
}

func (d *Difference) Error() string {
	return fmt.Sprintf("archive differs: file=%q offset=%d: %s", d.Path, d.Offset, d.Detail)
}

// Checker owns only temporary configuration and a symlink to the input mirror.
// The operator must quiesce external writers or use a filesystem snapshot: its
// Cache lock cannot coordinate with a separate running server process.
type Checker struct {
	cache *gitcache.Cache
	root  string
	repo  string
}

func New(repo string) (*Checker, error) {
	absolute, err := filepath.Abs(repo)
	if err != nil {
		return nil, err
	}
	root, err := os.MkdirTemp("", "oberth-archive-check-")
	if err != nil {
		return nil, err
	}
	c := &Checker{root: root, repo: absolute}
	if err := os.Symlink(absolute, filepath.Join(root, "mirror.git")); err != nil {
		c.Close()
		return nil, err
	}
	c.cache, err = gitcache.New(gitcache.Config{Root: root, Backend: gitcache.BackendGoGit, CommandTimeout: 30 * time.Minute, Upstream: func(string) (string, error) { return "", errors.New("diagnostic must not fetch") }})
	if err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}
func (c *Checker) Close() { _ = os.RemoveAll(c.root) }
func Git(ctx context.Context, repo string, args ...string) ([]byte, error) {
	if len(args) == 0 {
		return nil, errors.New("Git subcommand required")
	}
	switch args[0] {
	case "rev-list", "archive", "ls-tree", "verify-pack", "fsck":
	default:
		return nil, fmt.Errorf("diagnostic refuses Git subcommand %q", args[0])
	}
	// #nosec G204 -- fixed Git binary and argument vectors; only read-only built-in subcommands above are accepted.
	command := exec.CommandContext(ctx, "git", append([]string{"--no-replace-objects", "-C", repo}, args...)...)
	command.Env = gitEnvironment()
	var stderr bytes.Buffer
	command.Stderr = &stderr
	out, err := command.Output()
	if err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", args[0], err, stderr.String())
	}
	return out, nil
}

// Check goes through Cache.ArchiveTar, including the server's repository lock,
// backend dispatcher, storage options and tree/blob streaming implementation.
func (c *Checker) Check(ctx context.Context, commit string) (result error) {
	if err := gitcache.ValidateSHA(commit); err != nil {
		return err
	}
	actual, err := os.CreateTemp(c.root, "go-*.tar")
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, actual.Close()); _ = os.Remove(actual.Name()) }()
	archiveErr := c.cache.ArchiveTar(ctx, "mirror", commit, actual)
	if _, err = actual.Seek(0, io.SeekStart); err != nil {
		return errors.Join(archiveErr, err)
	}
	expected, err := os.CreateTemp(c.root, "git-*.tar")
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, expected.Close()); _ = os.Remove(expected.Name()) }()
	// #nosec G204 -- fixed read-only archive subcommand, validated full SHA and operator-selected repository, no shell.
	command := exec.CommandContext(ctx, "git", "--no-replace-objects", "-C", c.repo, "archive", "--format=tar", commit)
	command.Env = gitEnvironment()
	command.Stdout = expected
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err = command.Run(); err != nil {
		return fmt.Errorf("git archive: %w: %s", err, stderr.String())
	}
	if _, err = expected.Seek(0, io.SeekStart); err != nil {
		return err
	}
	return errors.Join(compare(actual, expected), archiveErr)
}

func next(reader *tar.Reader) (*tar.Header, error) {
	for {
		header, err := reader.Next()
		if err != nil {
			return nil, err
		}
		if header.Typeflag != tar.TypeXGlobalHeader {
			return header, nil
		}
	}
}
func compare(actual, expected io.Reader) error {
	a, b := tar.NewReader(actual), tar.NewReader(expected)
	left, right := make([]byte, 32<<10), make([]byte, 32<<10)
	for {
		ah, ae := next(a)
		bh, be := next(b)
		if errors.Is(ae, io.EOF) && errors.Is(be, io.EOF) {
			return nil
		}
		if ae != nil || be != nil {
			name := ""
			if bh != nil {
				name = bh.Name
			} else if ah != nil {
				name = ah.Name
			}
			return &Difference{Path: name, Offset: 0, Detail: fmt.Sprintf("entry read: go=%v git=%v", ae, be)}
		}
		if ah.Name != bh.Name || ah.Size != bh.Size || ah.Mode != bh.Mode || ah.Typeflag != bh.Typeflag || ah.Linkname != bh.Linkname {
			return &Difference{Path: bh.Name, Offset: 0, Detail: fmt.Sprintf("header mismatch: go=(%s %d %o %d %q) git=(%s %d %o %d %q)", ah.Name, ah.Size, ah.Mode, ah.Typeflag, ah.Linkname, bh.Name, bh.Size, bh.Mode, bh.Typeflag, bh.Linkname)}
		}
		for offset := int64(0); ; {
			an, ae := io.ReadFull(a, left)
			bn, be := io.ReadFull(b, right)
			for i := range min(an, bn) {
				if left[i] != right[i] {
					return &Difference{Path: bh.Name, Offset: offset + int64(i), Detail: "file bytes differ"}
				}
			}
			if an != bn {
				return &Difference{Path: bh.Name, Offset: offset + int64(min(an, bn)), Detail: "file lengths differ"}
			}
			offset += int64(an)
			if ae != nil || be != nil {
				if (errors.Is(ae, io.EOF) && errors.Is(be, io.EOF)) || (errors.Is(ae, io.ErrUnexpectedEOF) && errors.Is(be, io.ErrUnexpectedEOF)) {
					break
				}
				return &Difference{Path: bh.Name, Offset: offset, Detail: fmt.Sprintf("body read: go=%v git=%v", ae, be)}
			}
		}
	}
}

// Explain prints object IDs and every matching pack/index location. Git's
// verify-pack identifies each delta base; follow it until a non-delta object.
func Explain(ctx context.Context, repo, commit, path string, out io.Writer) error {
	absolute, err := filepath.Abs(repo)
	if err != nil {
		return err
	}
	repo = absolute
	tree, err := Git(ctx, repo, "ls-tree", "-z", commit, "--", path)
	if err != nil {
		return err
	}
	record, _, _ := strings.Cut(string(tree), "\x00")
	metadata, _, _ := strings.Cut(record, "\t")
	fields := strings.Fields(metadata)
	if len(fields) != 3 {
		return fmt.Errorf("no tree object for %q", path)
	}
	hash := fields[2]
	if _, err := fmt.Fprintf(out, "tree_entry=%s path=%q\n", metadata, path); err != nil {
		return err
	}
	indexes, err := filepath.Glob(filepath.Join(repo, "objects", "pack", "*.idx"))
	if err != nil {
		return err
	}
	type location struct {
		index  string
		fields []string
	}
	locations := map[string][]location{}
	for _, index := range indexes {
		body, err := Git(ctx, repo, "verify-pack", "-v", index)
		if err != nil {
			return err
		}
		for line := range strings.SplitSeq(string(body), "\n") {
			parts := strings.Fields(line)
			if len(parts) == 5 || len(parts) == 7 {
				if len(parts[0]) == 40 {
					locations[parts[0]] = append(locations[parts[0]], location{index, parts})
				}
			}
		}
	}
	seen := map[string]bool{}
	var explain func(string, int) error
	explain = func(hash string, depth int) error {
		if depth > 128 || seen[hash] {
			return fmt.Errorf("delta chain cycle/depth at %s", hash)
		}
		seen[hash] = true
		found := locations[hash]
		if len(found) == 0 {
			if _, err := fmt.Fprintf(out, "object=%s loose_or_alternate=true\n", hash); err != nil {
				return err
			}
			return nil
		}
		for _, location := range found {
			p := location.fields
			packPath := strings.TrimSuffix(location.index, ".idx") + ".pack"
			offset, err := strconv.ParseInt(p[4], 10, 64)
			if err != nil {
				return err
			}
			// #nosec G304 -- diagnostic explicitly reads the operator-selected bare mirror's pack files.
			pack, err := os.Open(packPath)
			if err != nil {
				return err
			}
			var header [1]byte
			_, readErr := pack.ReadAt(header[:], offset)
			closeErr := pack.Close()
			if err := errors.Join(readErr, closeErr); err != nil {
				return err
			}
			encoding := map[byte]string{1: "commit", 2: "tree", 3: "blob", 4: "tag", 6: "OFS_DELTA", 7: "REF_DELTA"}[(header[0]>>4)&7]
			if _, err := fmt.Fprintf(out, "object=%s pack=%q type=%s encoding=%s pack_entry_size=%s packed_size=%s offset=%s", hash, packPath, p[1], encoding, p[2], p[3], p[4]); err != nil {
				return err
			}
			if len(p) == 7 {
				if _, err := fmt.Fprintf(out, " depth=%s base=%s\n", p[5], p[6]); err != nil {
					return err
				}
				if !seen[p[6]] {
					if err := explain(p[6], depth+1); err != nil {
						return err
					}
				}
			} else {
				if _, err := fmt.Fprintln(out); err != nil {
					return err
				}
			}
		}
		return nil
	}
	return explain(hash, 0)
}

func gitEnvironment() []string {
	env := []string{}
	for _, value := range os.Environ() {
		if !strings.HasPrefix(value, "GIT_") {
			env = append(env, value)
		}
	}
	return append(env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_NO_REPLACE_OBJECTS=1", "GIT_TERMINAL_PROMPT=0")
}
