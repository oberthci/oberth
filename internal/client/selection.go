package client

import (
	"errors"
	"os"
)

// SelectedProfile reads the same profile used by transport resolution.
func SelectedProfile(base, name string) (Profile, string, error) {
	return loadProfile(base, name)
}

// ProfileSelection reserves the final local write before an installation applies.
type ProfileSelection struct {
	root    *os.Root
	temp    string
	name    string
	created []selectionDirectory
}

// PrepareProfileSelection starts approved publication and creates missing private bases.
func PrepareProfileSelection(base, name string) (*ProfileSelection, error) {
	created, err := createSelectionDirectories(base)
	if err != nil {
		return nil, errors.Join(err, removeSelectionDirectories(created))
	}
	root, temp, err := reserveSelection(base, name)
	if err != nil {
		return nil, errors.Join(err, removeSelectionDirectories(created))
	}
	return &ProfileSelection{root: root, temp: temp, name: name, created: created}, nil
}

func (s *ProfileSelection) Commit() error {
	if _, _, err := loadProfile(s.root.Name(), s.name); err != nil {
		return err
	}
	return commitJoinSelection(s.root, s.temp, false)
}
func (s *ProfileSelection) Close() error {
	err := s.root.Remove(s.temp)
	if errors.Is(err, os.ErrNotExist) {
		err = nil
	}
	return errors.Join(err, s.root.Close(), removeSelectionDirectories(s.created))
}
