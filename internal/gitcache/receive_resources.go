package gitcache

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

type receiveFiles struct {
	owner    string
	rejected bool
	before   map[string]bool
	owned    map[string]os.FileInfo
}

// One receive slot owns newly created quarantines in this repository. The
// baseline excludes pre-existing debris; public ref locks come from its record.
func receiveResources(store string) *receiveFiles {
	before := make(map[string]bool)
	_ = filepath.WalkDir(filepath.Join(store, "refs"), func(p string, d fs.DirEntry, err error) error {
		if err == nil {
			before[p] = true
		}
		return err
	})
	paths, _ := filepath.Glob(filepath.Join(store, "objects", "tmp_objdir-incoming-*"))
	for _, p := range paths {
		before[p] = true
	}
	for _, p := range append(packedRefFiles(store), filepath.Join(store, "HEAD.lock")) {
		if _, err := os.Lstat(p); err == nil {
			before[p] = true
		}
	}
	return &receiveFiles{before: before, owned: make(map[string]os.FileInfo)}
}

// Preparation holds the repository gate and names this native transaction's lock files.
// Retain identities so a replacement belonging to another writer is untouched.
func (files *receiveFiles) captureReferences(store string, updates []RefUpdate) {
	var paths []string
	deletes := false
	head, _ := os.ReadFile(filepath.Join(store, "HEAD"))
	for _, update := range updates {
		deletes = deletes || update.NewSHA == ""
		paths = append(paths, filepath.Join(store, update.Ref)+".lock")
		if strings.TrimSpace(string(head)) == "ref: "+update.Ref {
			paths = append(paths, filepath.Join(store, "HEAD.lock"))
		}
	}
	if deletes {
		paths = append(paths, packedRefFiles(store)...)
	}
	files.capture(paths)
}

func (files *receiveFiles) captureQuarantines(store string) {
	paths, _ := filepath.Glob(filepath.Join(store, "objects", "tmp_objdir-incoming-*"))
	files.capture(paths)
}

func (files *receiveFiles) capture(paths []string) {
	for _, path := range paths {
		if !files.before[path] {
			if info, err := os.Lstat(path); err == nil {
				files.owned[path] = info
			}
		}
	}
}

func (files *receiveFiles) cleanReceive() error {
	var result error
	for path, owned := range files.owned {
		current, err := os.Lstat(path)
		if err == nil && os.SameFile(owned, current) {
			if current.IsDir() {
				err = os.RemoveAll(path)
			} else {
				err = os.Remove(path)
			}
		}
		if err == nil || os.IsNotExist(err) {
			delete(files.owned, path)
		} else {
			result = errors.Join(result, err)
		}
	}
	if result != nil {
		return &receiveRollbackError{unavailable("clean receive files", result)}
	}
	return nil
}
