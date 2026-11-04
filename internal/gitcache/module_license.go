package gitcache

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"

	modzip "golang.org/x/mod/zip"

	"github.com/oberthci/oberth/internal/boundedio"
)

// ErrModuleLicenseTooLarge means an inherited LICENSE exceeds Go's zip limit.
var ErrModuleLicenseTooLarge = errors.New("module root LICENSE exceeds the size limit")

// ModuleRootLicense reads the repository-root LICENSE blob at the pinned commit.
// A Git symlink is a blob containing its link text; it is never dereferenced.
// A missing entry or a non-blob (for example a directory) has no license bytes.
// Object corruption, missing commits and IO errors fail closed.
func (c *Cache) ModuleRootLicense(ctx context.Context, input, source string) ([]byte, bool, error) {
	if err := ValidateSHA(source); err != nil {
		return nil, false, err
	}
	_, path, err := c.modulePath(ctx, input)
	if err != nil {
		return nil, false, err
	}
	unlock, err := c.lockModuleRepository(ctx, path)
	if err != nil {
		return nil, false, err
	}
	defer unlock()
	if !c.isBare(ctx, path) {
		return nil, false, errors.New("module repository is not cached")
	}
	entry := &boundedio.Buffer{Limit: 256, LimitError: errBlobTooLarge}
	if err := c.run(ctx, commandSpec{dir: path, args: []string{"--literal-pathspecs", "ls-tree", "-z", "--full-tree", source, "--", "LICENSE"}, stdout: entry}); err != nil {
		return nil, false, c.classifyModuleReadFailure(ctx, path, source, "LICENSE", err)
	}
	if len(entry.Bytes()) == 0 {
		return nil, false, nil
	}
	records := bytes.Split(entry.Bytes(), []byte{0})
	if len(records) != 2 || len(records[1]) != 0 {
		return nil, false, errors.New("invalid root LICENSE tree entry")
	}
	metadata, name, ok := strings.Cut(string(records[0]), "\t")
	fields := strings.Fields(metadata)
	if !ok || name != "LICENSE" || len(fields) != 3 || ValidateSHA(fields[2]) != nil {
		return nil, false, errors.New("invalid root LICENSE tree entry")
	}
	if fields[1] != "blob" {
		return nil, false, nil
	}
	if fields[0] != "100644" && fields[0] != "100755" && fields[0] != "120000" {
		return nil, false, errors.New("invalid root LICENSE blob mode")
	}
	body := &boundedio.Buffer{Limit: modzip.MaxLICENSE, LimitError: ErrModuleLicenseTooLarge}
	err = c.run(ctx, commandSpec{dir: path, args: []string{"cat-file", "blob", fields[2]}, stdout: body})
	if body.Err() != nil {
		return nil, false, fmt.Errorf("read root LICENSE: %w", body.Err())
	}
	if err != nil {
		return nil, false, err
	}
	return body.Bytes(), true, nil
}
