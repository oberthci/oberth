package gitcache

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ReceiveHookInvocation dispatches the two Git-owned hook symlinks to this
// executable. It is shared by the server main and the native test executable.
func ReceiveHookInvocation(ctx context.Context, arguments []string, input io.Reader, diagnostics io.Writer) (int, bool) {
	if len(arguments) == 0 {
		return 0, false
	}
	kind := filepath.Base(arguments[0])
	if kind != "pre-receive" && kind != "update" {
		return 0, false
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cache := newNativeHookCache()
	var err error
	if kind == "pre-receive" {
		if len(arguments) != 1 {
			err = errors.New("invalid pre-receive arguments")
		} else {
			err = cache.nativePreReceive(ctx, input)
		}
	} else if len(arguments) != 4 {
		err = errors.New("invalid update arguments")
	} else {
		err = cache.nativeUpdate(ctx, arguments[1], arguments[2], arguments[3])
	}
	if err != nil {
		_, _ = fmt.Fprintf(diagnostics, "oberth: rejected receive: %v\n", err)
		return 1, true
	}
	return 0, true
}

func newNativeHookCache() *Cache {
	env := make(map[string]string)
	for _, key := range []string{"GIT_DIR", "GIT_WORK_TREE", "GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_CONFIG_GLOBAL"} {
		if v := os.Getenv(key); v != "" {
			env[key] = v
		}
	}
	cache := &Cache{hookChild: true, gitBinary: "git", env: env, globalConfig: env["GIT_CONFIG_GLOBAL"], timeout: 30 * time.Second, logger: log.New(io.Discard, "", 0)}
	cache.backend = &cliBackend{cache: cache}
	return cache
}

func (c *Cache) nativePreReceive(ctx context.Context, input io.Reader) error {
	var output boundedGitInventory
	if err := c.run(ctx, commandSpec{args: []string{"for-each-ref", fmt.Sprintf("--count=%d", maximumPublicRefs+maximumReceiveRefUpdates+1), "--format=%(objectname) %(refname)", "refs/heads/", "refs/tags/"}, stdout: &output}); err != nil {
		return errors.New("cannot inspect current public refs")
	}
	current := make(map[string]string)
	for _, line := range strings.Split(strings.TrimSpace(output.String()), "\n") {
		if line == "" {
			continue
		}
		sha, ref, ok := strings.Cut(line, " ")
		if !ok || ValidateSHA(sha) != nil {
			return errors.New("invalid public-ref snapshot")
		}
		current[ref] = sha
	}
	count := len(current)
	seen := make(map[string]bool)
	scanner := bufio.NewScanner(io.LimitReader(input, int64((maximumReceiveRefUpdates+1)*(maximumRefPartBytes+128))))
	scanner.Buffer(make([]byte, 4096), maximumRefPartBytes+128)
	for scanner.Scan() {
		fields := strings.Split(scanner.Text(), " ")
		if len(fields) != 3 || ValidateSHA(fields[0]) != nil || ValidateSHA(fields[1]) != nil || fields[2] == "" {
			return errors.New("malformed ref update")
		}
		old, newSHA, ref := fields[0], fields[1], fields[2]
		if seen[ref] {
			return errors.New("duplicate update for " + ref)
		}
		seen[ref] = true
		if len(seen) > maximumReceiveRefUpdates {
			return fmt.Errorf("receive exceeds %d ref updates", maximumReceiveRefUpdates)
		}
		branch := strings.HasPrefix(ref, "refs/heads/")
		public := branch || strings.HasPrefix(ref, "refs/tags/")
		existing, exists := current[ref]
		if len(current) > maximumPublicRefs && (!branch || !zeroHookOID(newSHA) || !exists || old != existing) {
			return fmt.Errorf("repository exceeds %d public refs; recovery receive may contain only exact-current branch deletions", maximumPublicRefs)
		}
		if public {
			if zeroHookOID(newSHA) && branch && exists && old == existing {
				count--
			} else if !zeroHookOID(newSHA) && zeroHookOID(old) && !exists {
				count++
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return errors.New("malformed or overlarge receive commands")
	}
	if count > maximumPublicRefs {
		return fmt.Errorf("receive would leave %d public refs; limit is %d", count, maximumPublicRefs)
	}
	return nil
}
func zeroHookOID(value string) bool { return value == strings.Repeat("0", 40) }
func (c *Cache) nativeUpdate(ctx context.Context, ref, old, newSHA string) error {
	if ValidateSHA(old) != nil || ValidateSHA(newSHA) != nil {
		return errors.New("invalid update object ID")
	}
	if branch, ok := strings.CutPrefix(ref, "refs/heads/"); ok {
		if err := ValidateBranch(branch); err != nil {
			return err
		}
		if !zeroHookOID(old) && !zeroHookOID(newSHA) {
			head, err := c.capture(ctx, "", "symbolic-ref", "--quiet", "HEAD")
			if err == nil && strings.TrimSpace(head) == ref {
				if err := c.run(ctx, commandSpec{args: []string{"merge-base", "--is-ancestor", old, newSHA}}); err != nil {
					return errors.New("non-fast-forward push to default branch")
				}
			}
		}
		return nil
	}
	tag, ok := strings.CutPrefix(ref, "refs/tags/")
	if !ok {
		// Git receive.hideRefs fences clients; trusted local promotion writes
		// retain their existing ability to persist private refs.
		return nil
	}
	if err := ValidateTag(tag); err != nil {
		return err
	}
	if old == newSHA {
		return nil
	}
	if !zeroHookOID(old) {
		return errors.New("release tags are immutable")
	}
	if zeroHookOID(newSHA) {
		return errors.New("release tags cannot be deleted")
	}
	if err := c.run(ctx, commandSpec{args: []string{"show-ref", "--verify", "--quiet", upstreamRefPrefix + "tags/" + tag}}); err == nil {
		return errors.New("tag already exists upstream")
	}
	head, err := c.capture(ctx, "", "symbolic-ref", "--quiet", "HEAD")
	if err != nil {
		return errors.New("default branch is unavailable")
	}
	branch, ok := strings.CutPrefix(strings.TrimSpace(head), "refs/heads/")
	if !ok || ValidateBranch(branch) != nil {
		return errors.New("default branch is invalid")
	}
	commit, err := c.capture(ctx, "", "rev-parse", "--verify", newSHA+"^{commit}")
	if err != nil || ValidateSHA(strings.TrimSpace(commit)) != nil {
		return errors.New("tag does not select a commit")
	}
	if err := c.run(ctx, commandSpec{args: []string{"merge-base", "--is-ancestor", strings.TrimSpace(commit), releaseAdmissionRefPrefix + branch}}); err != nil {
		return fmt.Errorf("tag commit is not reachable from the fresh upstream default branch — promote the commit to the default branch first (promote <sha> %s), then tag", branch)
	}
	return nil
}

// installNativeHook atomically replaces legacy shell hooks with this binary.
func installNativeHook(hooks, name string) error {
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	target := filepath.Join(hooks, name)
	if link, err := os.Readlink(target); err == nil && link == executable {
		return nil
	}
	temp, err := os.CreateTemp(hooks, ".oberth-"+name+"-*")
	if err != nil {
		return err
	}
	temporary := temp.Name()
	if err := temp.Close(); err != nil {
		return errors.Join(err, os.Remove(temporary))
	}
	defer func() { _ = os.Remove(temporary) }()
	if err := os.Remove(temporary); err != nil {
		return err
	}
	if err := os.Symlink(executable, temporary); err != nil {
		return err
	}
	if err := os.Rename(temporary, target); err != nil {
		return err
	}
	return syncDirectory(hooks)
}

// Avoid relying on templates or any shell scripts in a minimal Git build.
func installReceiveHooks(repository string) error {
	hooks := filepath.Join(repository, "hooks")
	if err := os.Mkdir(hooks, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	info, err := os.Lstat(hooks)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("git hooks path is not a directory")
	}
	for _, name := range []string{"pre-receive", "update"} {
		if err := installNativeHook(hooks, name); err != nil {
			return err
		}
	}
	return nil
}
