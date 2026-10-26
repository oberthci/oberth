package gitcache

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/storage"
	"github.com/go-git/go-git/v5/storage/filesystem"
)

// go-git's default filesystem ref writer truncates a live ref in place and
// uses flock rather than Git's .lock protocol. Preserve atomic readers and
// CLI interoperability during the transition with lock/write/fsync/rename.
type atomicReferenceStore struct {
	*filesystem.Storage
	dir string
}

func wrapGoReferences(r *git.Repository, dir string) {
	if _, ok := r.Storer.(*atomicReferenceStore); !ok {
		r.Storer = &atomicReferenceStore{Storage: r.Storer.(*filesystem.Storage), dir: dir}
	}
}
func (s *atomicReferenceStore) SetReference(ref *plumbing.Reference) error {
	return s.CheckAndSetReference(ref, nil)
}
func (s *atomicReferenceStore) CheckAndSetReference(ref, old *plumbing.Reference) error {
	return s.writeReference(ref.Name(), ref, old)
}
func (s *atomicReferenceStore) RemoveReference(name plumbing.ReferenceName) error {
	return s.writeReference(name, nil, nil)
}
func (s *atomicReferenceStore) deleteReference(name plumbing.ReferenceName, old *plumbing.Reference) error {
	return s.writeReference(name, nil, old)
}
func (s *atomicReferenceStore) writeReference(name plumbing.ReferenceName, ref, old *plumbing.Reference) (result error) {
	if err := validateStorageRef(name); err != nil {
		return err
	}
	path := filepath.Join(s.dir, filepath.FromSlash(name.String()))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	// #nosec G304 -- repository-owned directory plus a validated storage ref or the fixed packed-refs filename.
	lock, err := os.OpenFile(path+".lock", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("lock ref %s: %w", name, err)
	}
	defer func() { result = errors.Join(result, lock.Close()); _ = os.Remove(path + ".lock") }()
	if old != nil {
		current, err := s.Reference(name)
		if errors.Is(err, plumbing.ErrReferenceNotFound) {
			if !old.Hash().IsZero() {
				return storage.ErrReferenceHasChanged
			}
		} else if err != nil {
			return err
		} else if old.Hash().IsZero() || current.Type() != old.Type() || current.String() != old.String() {
			return storage.ErrReferenceHasChanged
		}
	}
	if ref == nil {
		if err := s.removePackedReference(name); err != nil {
			return err
		}
		err := os.Remove(path)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	content := ref.Hash().String() + "\n"
	if ref.Type() == plumbing.SymbolicReference {
		content = "ref: " + ref.Target().String() + "\n"
	}
	if _, err := lock.WriteString(content); err != nil {
		return err
	}
	if err := lock.Sync(); err != nil {
		return err
	}
	return os.Rename(path+".lock", path)
}
func (s *atomicReferenceStore) removePackedReference(name plumbing.ReferenceName) (result error) {
	path := filepath.Join(s.dir, "packed-refs")
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	// #nosec G304 -- repository-owned directory plus a validated storage ref or the fixed packed-refs filename.
	lock, err := os.OpenFile(path+".lock", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, lock.Close()); _ = os.Remove(path + ".lock") }()
	// #nosec G304 -- fixed packed-refs file under the validated repository directory.
	body, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var out bytes.Buffer
	removed := false
	for _, line := range strings.SplitAfter(string(body), "\n") {
		if strings.HasPrefix(line, "^") && removed {
			continue
		}
		removed = false
		parts := strings.Fields(line)
		if len(parts) == 2 && parts[1] == name.String() {
			removed = true
			continue
		}
		out.WriteString(line)
	}
	if _, err := lock.Write(out.Bytes()); err != nil {
		return err
	}
	if err := lock.Sync(); err != nil {
		return err
	}
	return os.Rename(path+".lock", path)
}

func validateStorageRef(name plumbing.ReferenceName) error {
	// Git accepts leading dashes in later ref components. Public admission
	// rejects them separately, but legacy sanitation must still delete them.
	value := name.String()
	if name == plumbing.HEAD || value == "FETCH_HEAD" {
		return nil
	}
	if !strings.HasPrefix(value, "refs/") || strings.HasSuffix(value, "/") || strings.HasSuffix(value, ".") || strings.Contains(value, "..") || strings.Contains(value, "@{") || strings.ContainsAny(value, " ~^:?*[\\\x00\r\n") {
		return errors.New("invalid reference name")
	}
	for _, part := range strings.Split(value, "/") {
		if part == "" || strings.HasPrefix(part, ".") || strings.HasSuffix(part, ".lock") {
			return errors.New("invalid reference name")
		}
	}
	for _, r := range value {
		if r < 32 || r == 127 {
			return errors.New("invalid reference name")
		}
	}
	return nil
}
