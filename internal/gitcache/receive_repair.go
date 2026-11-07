package gitcache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Operator repair for one durable receive record. A quarantined or stuck
// record freezes receive and refresh for its repository, so an admin can
// show it, retry its delivery, or move it aside with an audit entry. The
// original bytes are never edited in place: retry rewrites a validated copy
// and abandon moves the file into the abandoned/ subdirectory.

const abandonedReceiveDirectory = "abandoned"

// operatorQuarantineState is the documented escape hatch: an operator sets a
// record's state to this value so startup skips it.
const operatorQuarantineState = "quarantined"

type ReceiveRecord struct {
	Repo    string      `json:"repo"`
	ID      string      `json:"id,omitempty"`
	Actor   string      `json:"actor,omitempty"`
	State   string      `json:"state,omitempty"`
	Next    int         `json:"next"`
	Updates []RefUpdate `json:"updates,omitempty"`
	Corrupt bool        `json:"corrupt"`
	Problem string      `json:"problem,omitempty"`
	// Retryable reports whether retry can deliver this record: a readable
	// record, or one whose only defect is the operator quarantine state.
	Retryable bool   `json:"retryable"`
	MovedTo   string `json:"moved_to,omitempty"`
}

// InspectReceiveRecord reads one record without changing it. A record that
// fails strict validation is decoded leniently so its contents can be shown.
func (c *Cache) InspectReceiveRecord(input string) (ReceiveRecord, error) {
	identity, err := c.recordIdentity(input)
	if err != nil {
		return ReceiveRecord{}, err
	}
	record, _, err := c.inspectRecord(identity)
	return record, err
}

// RetryReceiveRecord delivers a stuck record through handler under the
// repository's receive lock. An operator-quarantined record is first restored
// to its delivery state, but only when the result validates; any other
// corruption is refused and must be abandoned instead.
func (c *Cache) RetryReceiveRecord(ctx context.Context, input, id string, handler ReceiveHandler) (ReceiveRecord, error) {
	if handler == nil {
		return ReceiveRecord{}, errors.New("receive handler is required")
	}
	identity, err := c.recordIdentity(input)
	if err != nil {
		return ReceiveRecord{}, err
	}
	_, path, err := c.path(identity)
	if err != nil {
		return ReceiveRecord{}, err
	}
	lock := c.receiveLock(path)
	if err := lock.LockContext(ctx); err != nil {
		return ReceiveRecord{}, err
	}
	defer lock.Unlock()
	record, reservation, err := c.inspectRecord(identity)
	if err != nil {
		return ReceiveRecord{}, err
	}
	if id != "" && record.ID != id {
		return record, fmt.Errorf("receive record for %s is %q, not %q", identity, record.ID, id)
	}
	if !record.Retryable {
		return record, fmt.Errorf("receive record for %s cannot be delivered (%s); abandon it instead", identity, record.Problem)
	}
	if record.Corrupt {
		if err := c.writeReservation(reservation); err != nil {
			return record, fmt.Errorf("restore quarantined receive record: %w", err)
		}
	}
	if err := c.replayRepoSerialized(ctx, identity, path, handler); err != nil {
		return record, fmt.Errorf("retry receive record for %s: %w", identity, err)
	}
	record.State = "delivered"
	return record, nil
}

// AbandonReceiveRecord moves a record out of the replay outbox so its
// repository accepts receives again. The file is kept under abandoned/ for
// evidence; the caller audits the returned contents.
func (c *Cache) AbandonReceiveRecord(ctx context.Context, input, id string) (ReceiveRecord, error) {
	identity, err := c.recordIdentity(input)
	if err != nil {
		return ReceiveRecord{}, err
	}
	_, path, err := c.path(identity)
	if err != nil {
		return ReceiveRecord{}, err
	}
	lock := c.receiveLock(path)
	if err := lock.LockContext(ctx); err != nil {
		return ReceiveRecord{}, err
	}
	defer lock.Unlock()
	record, _, err := c.inspectRecord(identity)
	if err != nil {
		return ReceiveRecord{}, err
	}
	if id != "" && record.ID != id {
		return record, fmt.Errorf("receive record for %s is %q, not %q", identity, record.ID, id)
	}
	source, err := c.reservationPath(identity)
	if err != nil {
		return record, err
	}
	directory := filepath.Join(c.outboxRoot, abandonedReceiveDirectory)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return record, err
	}
	target := filepath.Join(directory, fmt.Sprintf("%s.%d.json", strings.TrimSuffix(filepath.Base(source), ".json"), time.Now().UTC().UnixNano()))
	if err := os.Rename(source, target); err != nil {
		return record, fmt.Errorf("move receive record aside: %w", err)
	}
	if err := errors.Join(syncDirectory(directory), syncDirectory(c.outboxRoot)); err != nil {
		return record, err
	}
	record.MovedTo = filepath.Join(abandonedReceiveDirectory, filepath.Base(target))
	return record, nil
}

func (c *Cache) recordIdentity(input string) (string, error) {
	repo, path, err := c.path(input)
	if err != nil {
		return "", err
	}
	return c.reservationIdentity(repo, path), nil
}

// inspectRecord returns the display record and, for a retryable record, the
// reservation retry must write back.
func (c *Cache) inspectRecord(identity string) (ReceiveRecord, receiveReservation, error) {
	reservation, err := c.readReservation(identity)
	if err == nil {
		return recordFrom(identity, reservation, false, ""), reservation, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return ReceiveRecord{}, receiveReservation{}, fmt.Errorf("no receive record for %s", identity)
	}
	if !errors.Is(err, ErrCorruptReceiveReservation) {
		return ReceiveRecord{}, receiveReservation{}, err
	}
	problem := err.Error()
	lenient, readErr := c.readReservationLenient(identity)
	if readErr != nil {
		return ReceiveRecord{Repo: identity, Corrupt: true, Problem: problem}, receiveReservation{}, nil
	}
	record := recordFrom(identity, lenient, true, problem)
	if lenient.State == operatorQuarantineState {
		restored := lenient
		restored.State = receiveReady
		if len(restored.Updates) == 0 {
			// Not yet finalized: replay recomputes the diff from Before.
			restored.State = receiveReserved
		}
		if validateReservation(restored, identity) == nil {
			record.Retryable = true
			return record, restored, nil
		}
	}
	return record, receiveReservation{}, nil
}

func recordFrom(identity string, reservation receiveReservation, corrupt bool, problem string) ReceiveRecord {
	return ReceiveRecord{Repo: identity, ID: reservation.ID, Actor: reservation.Actor, State: reservation.State, Next: reservation.Next,
		Updates: reservation.Updates, Corrupt: corrupt, Problem: problem, Retryable: !corrupt}
}

func (c *Cache) readReservationLenient(identity string) (receiveReservation, error) {
	path, err := c.reservationPath(identity)
	if err != nil {
		return receiveReservation{}, err
	}
	root, err := os.OpenRoot(c.outboxRoot)
	if err != nil {
		return receiveReservation{}, err
	}
	defer func() { _ = root.Close() }()
	name := filepath.Base(path)
	info, err := root.Lstat(name)
	if err != nil {
		return receiveReservation{}, err
	}
	if !info.Mode().IsRegular() || info.Size() > 8<<20 {
		return receiveReservation{}, errors.New("record is not a bounded regular file")
	}
	file, err := root.Open(name)
	if err != nil {
		return receiveReservation{}, err
	}
	defer func() { _ = file.Close() }()
	var reservation receiveReservation
	if err := json.NewDecoder(io.LimitReader(file, 8<<20)).Decode(&reservation); err != nil {
		return receiveReservation{}, err
	}
	if len(reservation.Updates) > maximumReceiveRefUpdates {
		reservation.Updates = reservation.Updates[:maximumReceiveRefUpdates]
	}
	return reservation, nil
}
