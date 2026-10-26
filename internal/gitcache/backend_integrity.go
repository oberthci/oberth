package gitcache

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/storer"
)

// Keep transferred refs private until their object graph has been checked.
// A failed transfer, invalid graph or crash cannot install an unverified ref.
type stagedReferenceStore struct {
	*atomicReferenceStore
	updates map[plumbing.ReferenceName]*plumbing.Reference
	removed map[plumbing.ReferenceName]bool
}

func (s *stagedReferenceStore) SetReference(ref *plumbing.Reference) error {
	if err := validateStorageRef(ref.Name()); err != nil {
		return err
	}
	s.updates[ref.Name()] = ref
	delete(s.removed, ref.Name())
	return nil
}
func (s *stagedReferenceStore) CheckAndSetReference(ref, old *plumbing.Reference) error {
	if old != nil {
		current, err := s.Reference(old.Name())
		if err != nil {
			return err
		}
		if current.String() != old.String() {
			return errors.New("reference changed (CAS)")
		}
	}
	return s.SetReference(ref)
}
func (s *stagedReferenceStore) RemoveReference(name plumbing.ReferenceName) error {
	s.removed[name] = true
	delete(s.updates, name)
	return nil
}
func (s *stagedReferenceStore) Reference(name plumbing.ReferenceName) (*plumbing.Reference, error) {
	if s.removed[name] {
		return nil, plumbing.ErrReferenceNotFound
	}
	if ref, ok := s.updates[name]; ok {
		return ref, nil
	}
	return s.atomicReferenceStore.Reference(name)
}
func (s *stagedReferenceStore) IterReferences() (storer.ReferenceIter, error) {
	iter, err := s.atomicReferenceStore.IterReferences()
	if err != nil {
		return nil, err
	}
	defer iter.Close()
	refs := map[plumbing.ReferenceName]*plumbing.Reference{}
	if err := iter.ForEach(func(ref *plumbing.Reference) error {
		if !s.removed[ref.Name()] {
			refs[ref.Name()] = ref
		}
		return nil
	}); err != nil {
		return nil, err
	}
	for name, ref := range s.updates {
		refs[name] = ref
	}
	values := make([]*plumbing.Reference, 0, len(refs))
	for _, ref := range refs {
		values = append(values, ref)
	}
	return storer.NewReferenceSliceIter(values), nil
}
func (s *stagedReferenceStore) commit() error {
	for name := range s.removed {
		if err := s.atomicReferenceStore.RemoveReference(name); err != nil {
			return err
		}
	}
	for _, ref := range s.updates {
		if err := s.atomicReferenceStore.SetReference(ref); err != nil {
			return err
		}
	}
	return nil
}
func goKnownRoots(r *git.Repository) (map[plumbing.Hash]bool, error) {
	roots := map[plumbing.Hash]bool{}
	iter, err := r.References()
	if err != nil {
		return nil, err
	}
	defer iter.Close()
	err = iter.ForEach(func(ref *plumbing.Reference) error {
		if ref.Type() == plumbing.HashReference && !strings.HasPrefix(ref.Name().String(), "refs/replace/") {
			roots[ref.Hash()] = true
		}
		return nil
	})
	return roots, err
}

// As with the old transfer.fsckObjects/receive.fsckObjects settings, malformed
// commits, missing dependencies, invalid tree names/modes and cycles must fail
// before a transferred ref can become public. Blobs are streamed/hashed by the
// pack decoder; this walk checks graph connectivity without loading blob data.
func validateGoGraph(ctx context.Context, r *git.Repository, roots []plumbing.Hash, known map[plumbing.Hash]bool) error {
	type node struct {
		hash plumbing.Hash
		kind plumbing.ObjectType
	}
	queue := make([]node, 0, len(roots))
	for _, hash := range roots {
		queue = append(queue, node{hash, plumbing.AnyObject})
	}
	seen := map[node]bool{}
	for len(queue) > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		item := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		if known[item.hash] {
			if item.kind != plumbing.AnyObject {
				if _, err := r.Storer.EncodedObject(item.kind, item.hash); err != nil {
					return err
				}
			}
			continue
		}
		if seen[item] {
			continue
		}
		seen[item] = true
		encoded, err := r.Storer.EncodedObject(item.kind, item.hash)
		if err != nil {
			return fmt.Errorf("invalid Git graph at %s: %w", item.hash, err)
		}
		switch encoded.Type() {
		case plumbing.CommitObject:
			commit, err := r.CommitObject(item.hash)
			if err != nil {
				return err
			}
			if commit.TreeHash.IsZero() {
				return errors.New("commit has no tree")
			}
			queue = append(queue, node{commit.TreeHash, plumbing.TreeObject})
			for _, parent := range commit.ParentHashes {
				if parent == item.hash {
					return errors.New("cyclic commit")
				}
				queue = append(queue, node{parent, plumbing.CommitObject})
			}
		case plumbing.TreeObject:
			tree, err := r.TreeObject(item.hash)
			if err != nil {
				return err
			}
			names := map[string]bool{}
			for _, entry := range tree.Entries {
				if names[entry.Name] || entry.Name == "" || entry.Name == "." || entry.Name == ".." || strings.EqualFold(entry.Name, ".git") || strings.ContainsAny(entry.Name, "/\x00") {
					return errors.New("invalid Git tree path")
				}
				names[entry.Name] = true
				kind := plumbing.BlobObject
				switch entry.Mode {
				case filemode.Dir:
					kind = plumbing.TreeObject
				case filemode.Submodule:
					continue
				case filemode.Regular, filemode.Executable, filemode.Symlink, filemode.Deprecated:
				default:
					return errors.New("invalid Git tree mode")
				}
				queue = append(queue, node{entry.Hash, kind})
			}
		case plumbing.TagObject:
			tag, err := r.TagObject(item.hash)
			if err != nil {
				return err
			}
			if tag.Target == item.hash {
				return errors.New("cyclic tag")
			}
			queue = append(queue, node{tag.Target, tag.TargetType})
		case plumbing.BlobObject:
		default:
			return errors.New("invalid Git object type")
		}
	}
	return nil
}

var _ io.Closer = (*stagedReferenceStore)(nil)
