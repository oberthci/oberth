package gitcache

import (
	"context"
	"fmt"
	"github.com/oberthci/oberth/internal/boundedio"
	"strconv"
	"strings"

	"github.com/oberthci/oberth/internal/gitport"
	"github.com/oberthci/oberth/internal/sourcecontract"
)

func (c *Cache) ReadFile(ctx context.Context, r gitport.Revision, path string) (gitport.File, error) {
	if err := r.Validate(); err != nil {
		return gitport.File{}, refuse("revision")
	}
	if err := sourcecontract.ValidateFilePath(path); err != nil {
		return gitport.File{}, refuse("file path")
	}
	p, err := c.path(r.Repository)
	if err != nil {
		return gitport.File{}, err
	}
	if err = c.lockRead(ctx, p); err != nil {
		return gitport.File{}, err
	}
	defer c.unlock(p)
	if err = c.commit(ctx, p, r.Commit); err != nil {
		return gitport.File{}, err
	}
	data, err := c.capture(ctx, command{dir: p, args: []string{"ls-tree", "-z", r.Commit, "--", path}}, 1<<20)
	if err != nil {
		return gitport.File{}, err
	}
	if len(data) == 0 {
		return gitport.File{}, &gitport.AbsentError{Repository: r.Repository, Ref: r.Commit, Path: path}
	}
	return c.readBlobEntry(ctx, p, path, data)
}
func (c *Cache) readBlobEntry(ctx context.Context, p, path string, data []byte) (gitport.File, error) {
	header, name, ok := strings.Cut(strings.TrimSuffix(string(data), "\x00"), "\t")
	fields := strings.Fields(header)
	if !ok || name != path || len(fields) != 3 {
		return gitport.File{}, refuse("file mode")
	}
	if fields[1] != "blob" || fields[0] != "100644" && fields[0] != "100755" {
		return gitport.File{}, refuse("file mode")
	}
	body, err := c.blob(ctx, p, fields[2], sourcecontract.MaxFileBytes)
	mode, _ := strconv.ParseUint(fields[0], 8, 32)
	return gitport.File{Path: path, Blob: fields[2], Mode: uint32(mode), Bytes: body}, err
}

func (c *Cache) commit(ctx context.Context, p, oid string) error {
	data, err := c.capture(
		ctx,
		command{
			dir:   p,
			args:  []string{"cat-file", "--batch-check=%(objecttype)"},
			input: strings.NewReader(oid + "\n"),
		},
		128,
	)
	if err != nil {
		return err
	}
	if string(data) == oid+" missing" {
		return &gitport.AbsentError{Ref: oid}
	}
	if string(data) != "commit" {
		return refuse("commit type")
	}
	return nil
}
func (c *Cache) ResolveRef(ctx context.Context, repo, ref string) (gitport.Ref, error) {
	if err := sourcecontract.ValidatePublicRef(ref); err != nil {
		return gitport.Ref{}, refuse("ref")
	}
	p, err := c.path(repo)
	if err != nil {
		return gitport.Ref{}, err
	}
	if err = c.lockRead(ctx, p); err != nil {
		return gitport.Ref{}, err
	}
	defer c.unlock(p)
	return c.resolve(ctx, p, repo, ref)
}
func (c *Cache) resolve(ctx context.Context, p, repo, ref string) (gitport.Ref, error) {
	data, err := c.capture(
		ctx,
		command{
			dir:  p,
			args: []string{"for-each-ref", "--format=%(refname) %(objectname) %(*objectname)", ref},
		},
		1<<20,
	)
	if err != nil {
		return gitport.Ref{}, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 || f[0] != ref {
			continue
		}
		result := gitport.Ref{Name: ref, ObjectSHA: f[1]}
		commit, e := c.capture(
			ctx,
			command{
				dir:   p,
				args:  []string{"cat-file", "--batch-check=%(objectname) %(objecttype)"},
				input: strings.NewReader(result.ObjectSHA + "^{commit}\n"),
			},
			256,
		)
		if e != nil {
			return gitport.Ref{}, e
		}
		fields := strings.Fields(string(commit))
		if len(fields) != 2 {
			return gitport.Ref{}, refuse("ref object")
		}
		if fields[1] == "commit" {
			result.CommitSHA = fields[0]
		}
		if strings.HasPrefix(ref, "refs/heads/") && result.CommitSHA == "" {
			return gitport.Ref{}, refuse("branch commit")
		}

		return result, nil
	}
	return gitport.Ref{}, &gitport.AbsentError{Repository: repo, Ref: ref}
}
func (c *Cache) ListRefs(ctx context.Context, repo string) ([]gitport.Ref, error) {
	p, err := c.path(repo)
	if err != nil {
		return nil, err
	}
	if err = c.lockRead(ctx, p); err != nil {
		return nil, err
	}
	defer c.unlock(p)
	return c.list(ctx, p, repo)
}
func (c *Cache) list(ctx context.Context, p, repo string) ([]gitport.Ref, error) {
	args := []string{
		"for-each-ref",
		fmt.Sprintf("--count=%d", sourcecontract.MaxRefs+1),
		"--format=%(refname)%00%(objectname)%00%(objecttype)%00%(*objectname)%00%(*objecttype)",
		"refs/heads/",
		"refs/tags/",
	}
	data, err := c.capture(ctx, command{dir: p, args: args}, 2<<20)
	if err != nil {
		return nil, err
	}
	result, err := c.decodeRefs(ctx, p, repo, data)
	if err != nil {
		return nil, err
	}
	if len(result) > sourcecontract.MaxRefs {
		return nil, refCountError()
	}
	return result, nil
}
func (c *Cache) decodeRefs(ctx context.Context, p, repo string, data []byte) ([]gitport.Ref, error) {
	var result []gitport.Ref
	for _, line := range strings.Split(string(data), "\n") {
		if line == "" {
			continue
		}
		ref, nested, err := inventoryRef(line)
		if err != nil {
			return nil, err
		}
		if nested {
			ref, err = c.resolve(ctx, p, repo, ref.Name)
			if err != nil {
				return nil, err
			}
		}
		result = append(result, ref)
	}
	return result, nil
}

func inventoryRef(line string) (gitport.Ref, bool, error) {
	f := strings.Split(line, "\x00")
	if len(f) != 5 {
		return gitport.Ref{}, false, refuse("ref inventory")
	}
	ref := gitport.Ref{Name: f[0], ObjectSHA: f[1]}
	if f[2] == "commit" {
		ref.CommitSHA = f[1]
	}
	if f[4] == "commit" {
		ref.CommitSHA = f[3]
	}
	if err := ref.Validate(); err != nil {
		return ref, false, refuse("ref inventory")
	}
	return ref, f[4] == "tag", nil
}

func (c *Cache) blob(ctx context.Context, p, oid string, limit int64) ([]byte, error) {
	b := boundedio.New(int(limit))
	err := c.run(ctx, command{dir: p, args: []string{"cat-file", "blob", oid}, output: b})
	if b.Err() != nil {
		return nil, refuse("file size")
	}
	return b.Bytes(), err
}
