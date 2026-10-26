package gitcache

import (
	"bytes"
	"compress/zlib"
	"context"
	"crypto"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	githash "github.com/go-git/go-git/v5/plumbing/hash"
)

// Local cache/workspace transfers can copy immutable object files instead of
// unpacking and recompressing the entire mirror in the embedded file server.
// No hardlinks or alternates are created: workspace lifetime stays independent
// of cache GC. Copied pack/index/loose checksums are verified before rename;
// the caller still validates the selected graph before committing staged refs.
func fetchLocalObjects(ctx context.Context, r *git.Repository, source string, specs []config.RefSpec, prune bool) error {
	upstream, closeStorage, err := openGoRepository(source)
	if err != nil {
		return err
	}
	defer closeStorage()
	sourceStore := upstream.Storer.(*atomicReferenceStore)
	staged, ok := r.Storer.(*stagedReferenceStore)
	if !ok {
		return errors.New("local fetch requires staged refs")
	}
	sourceRefs := map[plumbing.ReferenceName]plumbing.Hash{}
	iter, err := upstream.References()
	if err != nil {
		return err
	}
	err = iter.ForEach(func(ref *plumbing.Reference) error {
		if ref.Type() == plumbing.HashReference {
			sourceRefs[ref.Name()] = ref.Hash()
		}
		return nil
	})
	iter.Close()
	if err != nil {
		return err
	}
	selected := map[plumbing.ReferenceName]plumbing.Hash{}
	for _, spec := range specs {
		matched := false
		if spec.IsExactSHA1() {
			hash := plumbing.NewHash(spec.Src())
			if _, err := upstream.Storer.EncodedObject(plumbing.AnyObject, hash); err != nil {
				return err
			}
			selected[spec.Dst("")] = hash
			matched = true
		} else {
			for name, hash := range sourceRefs {
				if spec.Match(name) {
					selected[spec.Dst(name)] = hash
					matched = true
				}
			}
		}
		if !matched && !spec.IsWildcard() {
			return fmt.Errorf("remote ref %s not found", spec.Src())
		}
	}
	if err := copyLocalObjectFiles(ctx, filepath.Join(sourceStore.dir, "objects"), filepath.Join(staged.dir, "objects")); err != nil {
		return err
	}
	if prune {
		current, err := staged.IterReferences()
		if err != nil {
			return err
		}
		err = current.ForEach(func(ref *plumbing.Reference) error {
			for _, spec := range specs {
				if spec.Reverse().Match(ref.Name()) {
					if _, ok := selected[ref.Name()]; !ok {
						return staged.RemoveReference(ref.Name())
					}
				}
			}
			return nil
		})
		current.Close()
		if err != nil {
			return err
		}
	}
	for name, hash := range selected {
		if err := staged.SetReference(plumbing.NewHashReference(name, hash)); err != nil {
			return err
		}
	}
	return nil
}
func copyLocalObjectFiles(ctx context.Context, source, destination string) error {
	entries, err := os.ReadDir(source)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		name := entry.Name()
		if name == "pack" && entry.IsDir() {
			packs, err := os.ReadDir(filepath.Join(source, name))
			if err != nil {
				return err
			}
			sort.SliceStable(packs, func(i, j int) bool {
				return filepath.Ext(packs[i].Name()) == ".pack" && filepath.Ext(packs[j].Name()) != ".pack"
			})
			for _, pack := range packs {
				file := pack.Name()
				extension := filepath.Ext(file)
				hash := strings.TrimSuffix(strings.TrimPrefix(file, "pack-"), extension)
				if pack.Type().IsRegular() && strings.HasPrefix(file, "pack-") && (extension == ".pack" || extension == ".idx") && ValidateSHA(hash) == nil {
					if err := copyVerifiedObject(ctx, filepath.Join(source, name, file), filepath.Join(destination, name, file), hash, extension); err != nil {
						return err
					}
				}
			}
			continue
		}
		if entry.IsDir() && len(name) == 2 {
			objects, err := os.ReadDir(filepath.Join(source, name))
			if err != nil {
				return err
			}
			for _, object := range objects {
				hash := name + object.Name()
				if object.Type().IsRegular() && ValidateSHA(hash) == nil {
					if err := copyVerifiedObject(ctx, filepath.Join(source, name, object.Name()), filepath.Join(destination, name, object.Name()), hash, "loose"); err != nil {
						return err
					}
				}
			}
		}
	}
	return nil
}
func copyVerifiedObject(ctx context.Context, source, destination, expected, kind string) (result error) {
	if info, err := os.Lstat(destination); err == nil {
		if !info.Mode().IsRegular() {
			return errors.New("object destination is not regular")
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return err
	}
	// #nosec G304 -- source/destination are server-owned repository object paths with validated hash filenames.
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer func() { _ = input.Close() }()
	output, err := os.CreateTemp(filepath.Dir(destination), ".oberth-object-*")
	if err != nil {
		return err
	}
	temporary := output.Name()
	defer func() { _ = output.Close(); _ = os.Remove(temporary) }()
	info, err := input.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("source object is not regular")
	}
	if kind == "loose" {
		if _, err := io.Copy(output, contextReader{ctx, input}); err != nil {
			return err
		}
		if _, err := output.Seek(0, io.SeekStart); err != nil {
			return err
		}
		reader, err := zlib.NewReader(output)
		if err != nil {
			return err
		}
		digest := githash.New(crypto.SHA1)
		_, copyErr := io.Copy(digest, contextReader{ctx, reader})
		closeErr := reader.Close()
		if copyErr != nil || closeErr != nil {
			return errors.Join(copyErr, closeErr)
		}
		if hex.EncodeToString(digest.Sum(nil)) != expected {
			return errors.New("loose object checksum mismatch")
		}
	} else {
		size := info.Size()
		if (kind == ".pack" && size < 32) || (kind == ".idx" && size < 40) {
			return errors.New("truncated Git pack/index")
		}
		digest := githash.New(crypto.SHA1)
		if _, err := io.Copy(io.MultiWriter(output, digest), contextReader{ctx, io.NewSectionReader(input, 0, size-20)}); err != nil {
			return err
		}
		footer := make([]byte, 20)
		if _, err := input.ReadAt(footer, size-20); err != nil {
			return err
		}
		if !bytes.Equal(digest.Sum(nil), footer) {
			return errors.New("pack/index checksum mismatch")
		}
		packHash := footer
		if kind == ".idx" {
			packHash = make([]byte, 20)
			if _, err := input.ReadAt(packHash, size-40); err != nil {
				return err
			}
		}
		if hex.EncodeToString(packHash) != expected {
			return errors.New("pack filename checksum mismatch")
		}
		if _, err := output.Write(footer); err != nil {
			return err
		}
	}
	if err := output.Sync(); err != nil {
		return err
	}
	if err := output.Close(); err != nil {
		return err
	}
	return os.Rename(temporary, destination)
}
