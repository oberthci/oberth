package runlog

import (
	"context"
	"slices"
	"strings"
	"sync/atomic"

	"github.com/oberthci/oberth/internal/publicerr"
)

func position(saved index, entry ContainerLog) int {
	for i, v := range saved.Containers {
		if v.PodUID == entry.PodUID && v.Container == entry.Container {
			return i
		}
	}
	return -1
}
func binding(entry ContainerLog) error {
	for _, value := range []string{entry.NodeID, entry.PodUID, entry.PodName, entry.Namespace, entry.Container} {
		if value == "" || len(value) > 1024 || strings.ContainsAny(value, "\x00\r\n") {
			return publicerr.New(publicerr.InvalidInput, incompleteNativeContainerIdentityReason, bindingRemedy, nil)
		}
	}
	return nil
}
func samePhysical(saved, entry ContainerLog) error {
	if saved.Namespace != entry.Namespace || saved.PodName != entry.PodName {
		return publicerr.New(publicerr.Conflict, containerPhysicalBindingChangedReason, physicalBindingRemedy, nil)
	}
	return nil
}

// Track retains verified identities and refines only the supplied parent node.
// Span, reason and capture state survive attribution changes. Unrelated nodes
// cannot take a receipt; an identical repeat changes nothing.
func (s *Store) Track(ctx context.Context, id string, entries []ContainerLog) error {
	if err := checkID(id); err != nil {
		return err
	}
	if err := s.lock(ctx); err != nil {
		return err
	}
	defer s.unlock()
	saved, err := s.load(id)
	if err != nil {
		return err
	}
	changed := false
	for _, entry := range entries {
		update, err := trackEntry(&saved, entry)
		if err != nil {
			return err
		}
		changed = changed || update
	}
	if changed {
		if err := s.save(id, saved); err != nil {
			return err
		}
	}
	for _, entry := range entries {
		if saved.Containers[position(saved, entry)].State == Following {
			s.following.Store(copyKey(id, entry), true)
		}
	}
	return nil
}

// KeepFollowing retires process-only availability absent from a verified snapshot.
// It performs no storage I/O and cannot revoke an active retained copy.
func (s *Store) KeepFollowing(id string, entries []ContainerLog) {
	s.following.Range(func(raw, _ any) bool {
		key := raw.(captureKey)
		if key.run == id && !slices.ContainsFunc(entries, func(entry ContainerLog) bool {
			return entry.PodUID == key.pod && entry.Container == key.container
		}) {
			s.following.Delete(key)
		}
		return true
	})
}

// Reservation permits one capture or a release before opening its source.
// It is process-owned and cannot be reconstructed from a durable receipt.
type Reservation struct {
	store *Store
	run   string
	entry ContainerLog
	used  atomic.Bool
}

// Reserve syncs the pending receipt before the caller opens the native source.
// Nil means this physical container has a pending or completed attempt, including
// one interrupted by a restart, and must never be reopened.
func (s *Store) Reserve(ctx context.Context, id string, entry ContainerLog) (*Reservation, error) {
	live := entry.State == Following
	if err := checkID(id); err != nil {
		return nil, err
	}
	if err := s.lock(ctx); err != nil {
		return nil, err
	}
	defer s.unlock()
	saved, err := s.load(id)
	if err != nil {
		return nil, err
	}
	updated, err := trackEntry(&saved, entry)
	if err != nil {
		return nil, err
	}
	at := position(saved, entry)
	if saved.Containers[at].State == Copying || saved.Containers[at].State == Captured {
		if updated {
			return nil, s.save(id, saved)
		}
		return nil, nil
	}
	value := saved.Containers[at]
	entry.State = value.State
	value.State = Copying
	value.Reason, value.Detail = pendingReason, pendingDetail
	saved.Containers[at] = value
	if err = s.save(id, saved); err != nil {
		return nil, err
	}
	s.active.Store(copyKey(id, entry), true)
	if live {
		s.following.Store(copyKey(id, entry), true)
	}
	return &Reservation{store: s, run: id, entry: entry}, nil
}

// Release makes an unstarted reservation available again. Calling it asserts
// that the source was never opened. A used or zero handle refuses the request.
func (r *Reservation) Release(ctx context.Context) error {
	if r == nil || r.store == nil || !r.used.CompareAndSwap(false, true) {
		return publicerr.New(publicerr.InvalidTransition, captureReservationAlreadyUsedReason, releaseRemedy, nil)
	}
	s := r.store
	defer s.active.Delete(copyKey(r.run, r.entry))
	if err := s.lock(ctx); err != nil {
		return err
	}
	defer s.unlock()
	saved, err := s.load(r.run)
	if err != nil {
		return err
	}
	at := position(saved, r.entry)
	if at < 0 {
		return storage(containerReservationDisappearedReason, nil)
	}
	saved.Containers[at].State = Waiting
	if r.entry.State == Following {
		saved.Containers[at].State = Following
	}
	saved.Containers[at].Reason, saved.Containers[at].Detail = "", ""
	return s.save(r.run, saved)
}

func trackEntry(saved *index, entry ContainerLog) (bool, error) {
	if err := binding(entry); err != nil {
		return false, err
	}
	at := position(*saved, entry)
	if at < 0 {
		entry.Start, entry.End, entry.Reason = saved.Size, saved.Size, ""
		if entry.State != Following {
			entry.State = Waiting
		}
		entry.ParentNodeID, entry.Detail = "", ""
		saved.Containers = append(saved.Containers, entry)
		return true, nil
	}
	return refineEntry(saved, at, entry)
}
func refineEntry(saved *index, at int, entry ContainerLog) (bool, error) {
	old := saved.Containers[at]
	if err := samePhysical(old, entry); err != nil {
		return false, err
	}
	if old.NodeID != entry.NodeID && old.NodeID != entry.ParentNodeID {
		return false, publicerr.New(publicerr.Conflict, containerParentRefinementUnverifiedReason, refinementRemedy, nil)
	}
	if old.State == Waiting && entry.State == Following {
		old.State = Following
	}
	old.NodeID, old.Step, old.Main = entry.NodeID, entry.Step, entry.Main
	if old == saved.Containers[at] {
		return false, nil
	}
	saved.Containers[at] = old
	return true, nil
}

type captureKey struct{ run, pod, container string }

func copyKey(run string, entry ContainerLog) captureKey {
	return captureKey{run, entry.PodUID, entry.Container}
}
func (s *Store) availability(run string, entries []ContainerLog) {
	for i := range entries {
		if entries[i].State == Following {
			if _, live := s.following.Load(copyKey(run, entries[i])); !live {
				entries[i].State, entries[i].Reason, entries[i].Detail = Interrupted, pendingReason, pendingDetail
			}
		}
		if entries[i].State != Copying {
			continue
		}
		if _, active := s.active.Load(copyKey(run, entries[i])); !active {
			entries[i].State = Interrupted
		}
	}
}
