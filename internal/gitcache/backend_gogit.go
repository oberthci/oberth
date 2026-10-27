package gitcache

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/storage/filesystem"
)

const BackendGoGit Backend = "go-git"

type goGitBackend struct{ cache *Cache }

type operationError struct {
	cause  error
	detail string
}

func (e *operationError) Error() string { return e.detail }
func (e *operationError) Unwrap() error { return e.cause }

type gitStatusError int

func (e gitStatusError) Error() string { return fmt.Sprintf("exit status %d", int(e)) }
func (e gitStatusError) ExitCode() int { return int(e) }

func (b *goGitBackend) execute(ctx context.Context, spec commandSpec, capture bool) (string, error) {
	var output tailBuffer
	output.max = maxCapturedOutput
	sink := spec.stdout
	if capture {
		sink = &output
	}
	if sink == nil {
		sink = io.Discard
	}
	args := spec.args
	// Global options are generated internally (identity, hidden refs). Policy
	// is implemented in Go rather than delegated to config files or hooks.
	values := map[string]string{}
	for len(args) > 0 {
		value := ""
		if args[0] == "-c" && len(args) > 1 {
			value = args[1]
			args = args[2:]
		} else if strings.HasPrefix(args[0], "-c") {
			value = strings.TrimPrefix(args[0], "-c")
			args = args[1:]
		} else {
			break
		}
		key, item, ok := strings.Cut(value, "=")
		if !ok {
			return "", errors.New("invalid internal Git configuration")
		}
		values[key] = item
	}
	if len(args) == 0 {
		return "", errors.New("empty Git operation")
	}
	b.cache.logger.Printf("git backend=go-git %s", b.cache.redactedCommand(spec.args))
	err := b.dispatch(ctx, spec, args, values, sink)
	if err != nil {
		if ctx.Err() != nil {
			err = errors.Join(context.Cause(ctx), err)
		}
		detail := b.cache.redactOutput(redactURL(err.Error()))
		if spec.stderr != nil {
			_, _ = fmt.Fprintln(spec.stderr, detail)
		}
		repository := spec.repository
		if repository == "" {
			repository = spec.dir
		}
		b.cache.logger.Printf("git command failed: backend=go-git subcommand=%q repository=%q", b.cache.redactOutput(gitSubcommand(spec.args)), b.cache.redactOutput(redactURL(repository)))
		return "", fmt.Errorf("git command failed: %w", &operationError{cause: err, detail: detail})
	}
	return output.String(), nil
}

func openGoRepository(path string) (*git.Repository, func(), error) {
	r, err := git.PlainOpen(path)
	if err != nil {
		return nil, func() {}, err
	}
	dir := r.Storer.(*filesystem.Storage).Filesystem().Root()
	wrapGoReferences(r, dir)
	closeStorage := func() {
		if closer, ok := r.Storer.(io.Closer); ok {
			_ = closer.Close()
		}
	}
	return r, closeStorage, nil
}

func (b *goGitBackend) dispatch(ctx context.Context, spec commandSpec, args []string, values map[string]string, out io.Writer) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	op, a := args[0], args[1:]
	if op == "init" {
		path := a[len(a)-1]
		bare := contains(a, "--bare")
		r, err := git.PlainInit(path, bare)
		if err != nil {
			return err
		}
		if closer, ok := r.Storer.(io.Closer); ok {
			defer func() { _ = closer.Close() }()
		}
		if !bare {
			path = filepath.Join(path, ".git")
		}
		return os.MkdirAll(filepath.Join(path, "hooks"), 0o700)
	}
	if op == "clone" {
		return b.clone(ctx, a)
	}
	if op == "ls-remote" {
		return b.lsRemote(ctx, spec.dir, a, out)
	}
	r, closeStorage, err := openGoRepository(spec.dir)
	if err != nil {
		return err
	}
	defer closeStorage()
	switch op {
	case "fetch":
		return b.fetch(ctx, r, a)
	case "push":
		return b.push(ctx, r, a)
	case "checkout":
		h, err := resolveGoObject(r, a[len(a)-1], true)
		if err != nil {
			return err
		}
		work, err := r.Worktree()
		if err != nil {
			return err
		}
		return work.Checkout(&git.CheckoutOptions{Hash: h, Force: true})
	case "remote":
		if len(a) < 2 {
			return errors.New("invalid remote operation")
		}
		if a[0] == "get-url" {
			remote, err := r.Remote(a[1])
			if err != nil {
				return err
			}
			_, err = fmt.Fprintln(out, remote.Config().URLs[0])
			return err
		}
		if len(a) != 3 {
			return errors.New("invalid remote update")
		}
		if err := ValidateUpstream(a[2]); err != nil {
			return err
		}
		if a[0] == "add" {
			_, err := r.CreateRemote(&config.RemoteConfig{Name: a[1], URLs: []string{a[2]}})
			return err
		}
		if a[0] != "set-url" {
			return errors.New("unsupported remote operation")
		}
		c, err := r.Config()
		if err != nil {
			return err
		}
		remote, ok := c.Remotes[a[1]]
		if !ok {
			return git.ErrRemoteNotFound
		}
		remote.URLs = []string{a[2]}
		return r.SetConfig(c)
	case "rev-parse":
		if contains(a, "--is-bare-repository") {
			_, err := r.Worktree()
			_, writeErr := fmt.Fprintln(out, errors.Is(err, git.ErrIsBareRepository))
			return writeErr
		}
		h, err := resolveGoObject(r, a[len(a)-1], false)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(out, h)
		return err
	case "symbolic-ref":
		if len(a) == 1 {
			ref, err := r.Reference(plumbing.ReferenceName(a[0]), false)
			if err != nil {
				return err
			}
			if ref.Type() != plumbing.SymbolicReference {
				return gitStatusError(1)
			}
			_, err = fmt.Fprintln(out, ref.Target())
			return err
		}
		if len(a) != 2 {
			return errors.New("invalid symbolic-ref")
		}
		return r.Storer.SetReference(plumbing.NewSymbolicReference(plumbing.ReferenceName(a[0]), plumbing.ReferenceName(a[1])))
	case "show-ref":
		_, err := r.Reference(plumbing.ReferenceName(a[len(a)-1]), true)
		if errors.Is(err, plumbing.ErrReferenceNotFound) {
			return gitStatusError(1)
		}
		return err
	case "for-each-ref":
		return goListRefs(ctx, r, a, out)
	case "update-ref":
		return goUpdateRef(r, a)
	case "cat-file", "show":
		return goCatFile(ctx, r, op, a, out)
	case "ls-tree":
		return goLsTree(r, a, out)
	case "merge-base":
		if len(a) != 3 || a[0] != "--is-ancestor" {
			return errors.New("unsupported merge-base request")
		}
		older, err := resolveGoObject(r, a[1], true)
		if err != nil {
			return err
		}
		newer, err := resolveGoObject(r, a[2], true)
		if err != nil {
			return err
		}
		yes, err := goIsAncestor(ctx, r, older, newer)
		if err != nil {
			return err
		}
		if !yes {
			return gitStatusError(1)
		}
		return nil
	case "archive":
		return goArchive(ctx, r, a[len(a)-1], out)
	case "gc":
		return b.maintain(ctx, r, contains(a, "--auto"))
	case "upload-pack", "receive-pack":
		return b.serve(ctx, r, op, spec)
	default:
		return fmt.Errorf("unsupported internal Go Git operation %q", op)
	}
}

func contains(a []string, value string) bool {
	for _, v := range a {
		if v == value {
			return true
		}
	}
	return false
}

func resolveGoObject(r *git.Repository, revision string, commit bool) (plumbing.Hash, error) {
	original := revision
	if strings.HasSuffix(revision, "^{commit}") {
		commit = true
		revision = strings.TrimSuffix(revision, "^{commit}")
	}
	revision = strings.TrimSuffix(revision, "^{object}")
	var h *plumbing.Hash
	var err error
	if ValidateSHA(revision) == nil {
		value := plumbing.NewHash(revision)
		h = &value
	} else {
		h, err = r.ResolveRevision(plumbing.Revision(revision))
	}
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("resolve %s: %w", original, err)
	}
	if !commit {
		if _, err := r.Storer.EncodedObject(plumbing.AnyObject, *h); err != nil {
			return plumbing.ZeroHash, fmt.Errorf("resolve %s: %w", original, err)
		}
		return *h, nil
	}
	seen := map[plumbing.Hash]bool{}
	for {
		if seen[*h] {
			return plumbing.ZeroHash, errors.New("cyclic annotated tag")
		}
		seen[*h] = true
		obj, err := r.Storer.EncodedObject(plumbing.AnyObject, *h)
		if err != nil {
			return plumbing.ZeroHash, err
		}
		if obj.Type() == plumbing.CommitObject {
			return *h, nil
		}
		if obj.Type() != plumbing.TagObject {
			return plumbing.ZeroHash, fmt.Errorf("%s does not select a commit", original)
		}
		tag, err := r.TagObject(*h)
		if err != nil {
			return plumbing.ZeroHash, err
		}
		h = &tag.Target
	}
}

func goListRefs(ctx context.Context, r *git.Repository, a []string, out io.Writer) error {
	count := int(^uint(0) >> 1)
	format := "%(refname) %(objectname)"
	var prefixes []string
	for _, arg := range a {
		switch {
		case strings.HasPrefix(arg, "--count="):
			n, err := strconv.Atoi(strings.TrimPrefix(arg, "--count="))
			if err != nil || n < 0 {
				return errors.New("invalid ref count")
			}
			count = n
		case strings.HasPrefix(arg, "--format="):
			format = strings.TrimPrefix(arg, "--format=")
		default:
			prefixes = append(prefixes, arg)
		}
	}
	refs, err := r.References()
	if err != nil {
		return err
	}
	defer refs.Close()
	var names []*plumbing.Reference
	err = refs.ForEach(func(ref *plumbing.Reference) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !strings.HasPrefix(ref.Name().String(), "refs/") {
			return nil
		}
		match := len(prefixes) == 0
		for _, p := range prefixes {
			if ref.Name().String() == p || strings.HasPrefix(ref.Name().String(), strings.TrimSuffix(p, "/")+"/") {
				match = true
			}
		}
		if match {
			names = append(names, ref)
		}
		return nil
	})
	if err != nil {
		return err
	}
	sort.Slice(names, func(i, j int) bool { return names[i].Name() < names[j].Name() })
	for i, ref := range names {
		if i >= count {
			break
		}
		line := strings.ReplaceAll(strings.ReplaceAll(format, "%(refname)", ref.Name().String()), "%(objectname)", ref.Hash().String())
		if format == "%(creatordate:iso-strict)" {
			tag, err := r.TagObject(ref.Hash())
			var date time.Time
			if err == nil {
				date = tag.Tagger.When
			} else {
				c, err := r.CommitObject(ref.Hash())
				if err != nil {
					return err
				}
				date = c.Committer.When
			}
			line = date.Format(time.RFC3339)
		}
		if _, err := fmt.Fprintln(out, line); err != nil {
			return err
		}
	}
	return nil
}

func goUpdateRef(r *git.Repository, a []string) error {
	deleting := a[0] == "-d"
	if deleting {
		a = a[1:]
	}
	name := plumbing.ReferenceName(a[0])
	if err := validateStorageRef(name); err != nil {
		return err
	}
	if deleting {
		if len(a) > 1 {
			current, err := r.Storer.Reference(name)
			if errors.Is(err, plumbing.ErrReferenceNotFound) && a[1] == strings.Repeat("0", 40) {
				return nil
			}
			if err != nil {
				return err
			}
			if current.Hash() != plumbing.NewHash(a[1]) {
				return errors.New("reference changed (CAS)")
			}
		}
		store, ok := r.Storer.(*atomicReferenceStore)
		if !ok {
			return errors.New("atomic ref store required")
		}
		var old *plumbing.Reference
		if len(a) > 1 {
			old = plumbing.NewHashReference(name, plumbing.NewHash(a[1]))
		}
		return store.deleteReference(name, old)
	}
	if len(a) < 2 {
		return errors.New("invalid update-ref")
	}
	var old *plumbing.Reference
	if len(a) > 2 {
		old = plumbing.NewHashReference(name, plumbing.NewHash(a[2]))
	}
	return r.Storer.CheckAndSetReference(plumbing.NewHashReference(name, plumbing.NewHash(a[1])), old)
}

func goIsAncestor(ctx context.Context, r *git.Repository, older, newer plumbing.Hash) (bool, error) {
	queue := []plumbing.Hash{newer}
	seen := map[plumbing.Hash]bool{}
	for len(queue) > 0 {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		h := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		if h == older {
			return true, nil
		}
		if seen[h] {
			continue
		}
		seen[h] = true
		c, err := r.CommitObject(h)
		if err != nil {
			return false, err
		}
		queue = append(queue, c.ParentHashes...)
	}
	return false, nil
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

func goBlob(r *git.Repository, revision string) (*object.Blob, error) {
	ref, path, hasPath := strings.Cut(revision, ":")
	h, err := resolveGoObject(r, ref, hasPath)
	if err != nil {
		return nil, err
	}
	if hasPath {
		commit, err := r.CommitObject(h)
		if err != nil {
			return nil, err
		}
		tree, err := commit.Tree()
		if err != nil {
			return nil, err
		}
		file, err := tree.File(path)
		if err != nil {
			return nil, err
		}
		return &file.Blob, nil
	}
	return r.BlobObject(h)
}
func goCatFile(ctx context.Context, r *git.Repository, op string, a []string, out io.Writer) error {
	revision := a[len(a)-1]
	if op == "show" && contains(a, "--format=%s") {
		h, err := resolveGoObject(r, revision, true)
		if err != nil {
			return err
		}
		commit, err := r.CommitObject(h)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(out, strings.SplitN(commit.Message, "\n", 2)[0])
		return err
	}
	if op == "cat-file" && (a[0] == "-e" || a[0] == "-t") {
		h, err := resolveGoObject(r, revision, false)
		if err != nil {
			return err
		}
		obj, err := r.Storer.EncodedObject(plumbing.AnyObject, h)
		if err != nil {
			return err
		}
		if a[0] == "-t" {
			_, err = fmt.Fprintln(out, obj.Type())
		}
		return err
	}
	blob, err := goBlob(r, revision)
	if err != nil {
		return err
	}
	reader, err := blob.Reader()
	if err != nil {
		return err
	}
	defer func() { _ = reader.Close() }()
	_, err = io.Copy(out, contextReader{ctx, reader})
	return err
}
func goLsTree(r *git.Repository, a []string, out io.Writer) error {
	h, err := resolveGoObject(r, a[1], true)
	if err != nil {
		return err
	}
	commit, err := r.CommitObject(h)
	if err != nil {
		return err
	}
	tree, err := commit.Tree()
	if err != nil {
		return err
	}
	name := strings.TrimPrefix(a[len(a)-1], ":(literal)")
	entry, err := tree.FindEntry(name)
	if errors.Is(err, object.ErrEntryNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	kind := "blob"
	if entry.Mode == filemode.Dir {
		kind = "tree"
	}
	if entry.Mode == filemode.Submodule {
		kind = "commit"
	}
	_, err = fmt.Fprintf(out, "%06o %s %s\t%s%c", entry.Mode, kind, entry.Hash, name, 0)
	return err
}
func goArchive(ctx context.Context, r *git.Repository, revision string, out io.Writer) (result error) {
	h, err := resolveGoObject(r, revision, true)
	if err != nil {
		return err
	}
	commit, err := r.CommitObject(h)
	if err != nil {
		return err
	}
	tree, err := commit.Tree()
	if err != nil {
		return err
	}
	if err := checkGoArchiveAttributes(ctx, r, tree); err != nil {
		return err
	}
	return goArchiveTree(ctx, r, commit, tree, out)
}

// Shadow alone may walk without export transformations, but must still consume
// every blob through verified storage, including blobs Git export-ignore omits.
func goArchiveTree(ctx context.Context, r *git.Repository, commit *object.Commit, tree *object.Tree, out io.Writer) (result error) {
	archive := tar.NewWriter(out)
	defer func() { result = errors.Join(result, archive.Close()) }()
	return walkGoTree(ctx, r, tree, func(name string, entry object.TreeEntry) error {
		header := &tar.Header{Name: name, Mode: 0o664, ModTime: commit.Committer.When}
		switch entry.Mode {
		case filemode.Dir:
			header.Typeflag = tar.TypeDir
			header.Name += "/"
			header.Mode = 0o775
		case filemode.Submodule:
			header.Typeflag = tar.TypeDir
			header.Name += "/"
			header.Mode = 0o775
		case filemode.Symlink:
			blob, err := r.BlobObject(entry.Hash)
			if err != nil {
				return err
			}
			reader, err := blob.Reader()
			if err != nil {
				return err
			}
			body, readErr := io.ReadAll(io.LimitReader(reader, 65537))
			closeErr := reader.Close()
			if readErr != nil || closeErr != nil {
				return errors.Join(readErr, closeErr)
			}
			if len(body) > 65536 {
				return errors.New("oversized archive symlink")
			}
			header.Typeflag = tar.TypeSymlink
			header.Linkname = string(body)
			header.Mode = 0o777
		case filemode.Regular, filemode.Executable, filemode.Deprecated:
			blob, err := r.BlobObject(entry.Hash)
			if err != nil {
				return err
			}
			header.Size = blob.Size
			if entry.Mode == filemode.Executable {
				header.Mode = 0o775
			}
		default:
			return errors.New("unsupported Git tree mode")
		}
		if err := archive.WriteHeader(header); err != nil {
			return err
		}
		if entry.Mode == filemode.Regular || entry.Mode == filemode.Executable || entry.Mode == filemode.Deprecated {
			blob, err := r.BlobObject(entry.Hash)
			if err != nil {
				return err
			}
			reader, err := blob.Reader()
			if err != nil {
				return err
			}
			_, copyErr := io.Copy(archive, contextReader{ctx, reader})
			closeErr := reader.Close()
			if copyErr != nil || closeErr != nil {
				return errors.Join(copyErr, closeErr)
			}
		}
		return nil
	})
}
