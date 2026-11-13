package gitcache

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/oberthci/oberth/internal/boundedio"
)

// These readers support cache parity and concurrency fixtures. Source delivery
// uses Checkout; the production module proxy and its readers have been removed.
// All repository operations join the same fair queue. Context-aware readers
// can leave it without a detached goroutine acquiring the lock later.
func (c *Cache) lockModuleRepository(ctx context.Context, path string) (func(), error) {
	lock := c.repoLock(path)
	if err := lock.RLockContext(ctx); err != nil {
		return nil, err
	}
	return lock.RUnlock, nil
}

// ListTags returns all tag names (without refs/tags/) in the cached bare
// repository.
func (c *Cache) ListTags(ctx context.Context, input string) ([]string, error) {
	_, path, err := c.modulePath(ctx, input)
	if err != nil {
		return nil, err
	}
	unlock, err := c.lockModuleRepository(ctx, path)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if !c.isBare(ctx, path) {
		return nil, fmt.Errorf("repository is not cached")
	}
	refs, err := c.listRefs(ctx, path, "refs/tags/")
	if err != nil {
		return nil, err
	}
	tags := make([]string, 0, len(refs))
	for ref := range refs {
		tags = append(tags, strings.TrimPrefix(ref, "refs/tags/"))
	}
	sort.Strings(tags)
	return tags, nil
}

// ShowFile returns the verbatim contents of a single file at the given ref.
// The bytes are returned
// exactly as stored in the git tree — no trimming or normalisation — because
// Go's checksum verification (go.sum /go.mod h1: lines) hashes the exact
// file bytes and any whitespace change breaks the hash (#726).
func (c *Cache) ShowFile(ctx context.Context, input, ref, filePath string) ([]byte, error) {
	if err := validateModuleRef(ref); err != nil {
		return nil, err
	}
	if err := validateBlobPath(filePath); err != nil {
		return nil, err
	}
	_, repoPath, err := c.modulePath(ctx, input)
	if err != nil {
		return nil, err
	}
	unlock, err := c.lockModuleRepository(ctx, repoPath)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if !c.isBare(ctx, repoPath) {
		return nil, fmt.Errorf("repository is not cached")
	}
	// Use run() with a raw bytes.Buffer instead of capture(): capture()
	// returns via tailBuffer.String() which applies bytes.TrimSpace,
	// stripping trailing newlines. That is correct for SHA/URL/ref output
	// but destroys the go.mod trailing newline that go.sum hashes over.
	var buf bytes.Buffer
	if err := c.run(ctx, commandSpec{
		dir:    repoPath,
		args:   []string{"show", "--end-of-options", ref + ":" + filePath, "--"},
		stdout: &buf,
	}); err != nil {
		return nil, c.classifyModuleReadFailure(ctx, repoPath, ref, filePath, err)
	}
	return buf.Bytes(), nil
}

// ArchiveTar writes a tar archive of the tree at the given ref.
func (c *Cache) ArchiveTar(ctx context.Context, input, ref string, w io.Writer) error {
	if err := validateModuleRef(ref); err != nil {
		return err
	}
	_, repoPath, err := c.modulePath(ctx, input)
	if err != nil {
		return err
	}
	unlock, err := c.lockModuleRepository(ctx, repoPath)
	if err != nil {
		return err
	}
	defer unlock()
	if !c.isBare(ctx, repoPath) {
		return fmt.Errorf("repository is not cached")
	}
	err = c.run(ctx, commandSpec{
		dir:    repoPath,
		args:   []string{"archive", "--format=tar", "--", ref},
		stdout: w,
	})
	if err != nil {
		return c.classifyModuleReadFailure(ctx, repoPath, ref, "", err)
	}
	return nil
}

func validateModuleRef(ref string) error {
	if ValidateSHA(ref) == nil {
		return nil
	}
	if strings.HasPrefix(ref, "refs/heads/") {
		return ValidateBranch(strings.TrimPrefix(ref, "refs/heads/"))
	}
	if strings.HasPrefix(ref, "refs/tags/") {
		return ValidateTag(strings.TrimPrefix(ref, "refs/tags/"))
	}
	return ValidateTag(ref)
}

func (c *Cache) modulePath(ctx context.Context, input string) (string, string, error) {
	if err := ctx.Err(); err != nil {
		return "", "", err
	}
	if c.qualifyContext != nil {
		return c.pathWithQualifier(input, func(input string) (RepoQualification, error) { return c.qualifyContext(ctx, input) })
	}
	return c.path(input)
}

// ErrModuleSourceMissing means a pinned commit is positively absent locally.
// Retrying the same read cannot reconstruct a pruned object or rebind its tag.
var ErrModuleSourceMissing = errors.New("pinned module commit is not cached")

// ErrModuleFileMissing means the pinned commit exists but the requested file does not.
var ErrModuleFileMissing = errors.New("file is absent from the pinned module commit")

// Called with the repository lock held, only after a failed pinned read. Use
// cat-file's machine-readable missing response, never localized stderr or an
// exit status that could also denote a transient process/filesystem failure.
func (c *Cache) classifyModuleReadFailure(ctx context.Context, path, ref, filePath string, cause error) error {
	if ctx.Err() != nil || ValidateSHA(ref) != nil {
		return cause
	}
	query := ref + "^{commit}"
	output := &boundedio.Buffer{Limit: 256, LimitError: errBlobTooLarge}
	if err := c.run(ctx, commandSpec{dir: path, args: []string{"cat-file", "--batch-check=%(objectname) %(objecttype)"}, stdin: strings.NewReader(query + "\n"), stdout: output}); err != nil {
		return cause
	}
	if strings.TrimSpace(string(output.Bytes())) == query+" missing" {
		return fmt.Errorf("%w: %w", ErrModuleSourceMissing, cause)
	}
	fields := strings.Fields(string(output.Bytes()))
	if filePath == "" || len(fields) != 2 || ValidateSHA(fields[0]) != nil || fields[1] != "commit" {
		return cause
	}
	// Check the tree entry, not cat-file's path lookup: a missing stored blob
	// is an IO failure even though cat-file would also report "missing".
	output = &boundedio.Buffer{Limit: len(filePath) + 128, LimitError: errBlobTooLarge}
	if err := c.run(ctx, commandSpec{dir: path, args: []string{"--literal-pathspecs", "ls-tree", "-z", "--full-tree", fields[0], "--", filePath}, stdout: output}); err != nil {
		return cause
	}
	if len(output.Bytes()) == 0 {
		return fmt.Errorf("%w: %w", ErrModuleFileMissing, cause)
	}
	return cause
}
