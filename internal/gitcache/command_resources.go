package gitcache

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Paths are recorded before launch. Ref writers run under the repository gate;
// their explicit ref locks cannot belong to an overlapping quarantine reader.
type commandFiles []string

func commandResources(s command) commandFiles {
	if s.hooks || !localRefWriter(s.args) {
		return nil
	}
	candidates := transactionResources(s)
	if nativeOperation(s.args) == "gc" {
		candidates = append(candidates, packedRefFiles(s.dir)...)
		candidates = append(candidates,
			filepath.Join(s.dir, "HEAD.lock"),
			filepath.Join(s.dir, "gc.pid.lock"))
		_ = filepath.WalkDir(filepath.Join(s.dir, "refs"), func(p string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() && !strings.HasSuffix(p, ".lock") {
				candidates = append(candidates, p+".lock")
			}
			return err
		})
	}
	return absentFiles(candidates)
}

func transactionResources(s command) []string {
	var candidates []string
	head, _ := os.ReadFile(filepath.Join(s.dir, "HEAD"))
	current := strings.TrimSpace(strings.TrimPrefix(string(head), "ref: "))
	for _, name := range s.refWrites {
		candidates = append(candidates, filepath.Join(s.dir, name)+".lock")
		if name == current {
			candidates = append(candidates, filepath.Join(s.dir, "HEAD.lock"))
		}
	}
	if s.refDeletes {
		candidates = append(candidates, packedRefFiles(s.dir)...)
	}
	return candidates
}

func absentFiles(paths []string) commandFiles {
	var owned commandFiles
	for _, p := range paths {
		if _, err := os.Lstat(p); os.IsNotExist(err) {
			owned = append(owned, p)
		}
	}
	return owned
}
func (paths commandFiles) clean() error {
	var result error
	for _, p := range paths {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			result = errors.Join(result, err)
		}
	}
	return result
}

func packedRefFiles(store string) []string {
	return []string{filepath.Join(store, "packed-refs.lock"), filepath.Join(store, "packed-refs.new")}
}
