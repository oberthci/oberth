package gitcache

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const receiveReservationVersion = 2

const (
	receiveReserved = "reserved"
	receiveReady    = "ready"
)

type receiveReservation struct {
	Version          int               `json:"version"`
	ID               string            `json:"id"`
	Repo             string            `json:"repo"`
	Actor            string            `json:"actor"`
	State            string            `json:"state"`
	Before           map[string]string `json:"before"`
	Updates          []RefUpdate       `json:"updates,omitempty"`
	Next             int               `json:"next"`
	ReleaseAdmission ReleaseAdmission  `json:"release_admission"`
}

// Receive serializes the complete actor-bound receive transaction for one
// repository. A durable reservation exists before receive-pack begins; after
// Git publishes refs it is finalized into replayable events before callbacks
// run. A callback failure leaves the stable event in the PVC outbox.
func (c *Cache) Receive(ctx context.Context, input, actor, protocol string, stdin io.Reader, stdout, stderr io.Writer, handler ReceiveHandler) error {
	repo, path, err := c.path(input)
	if err != nil {
		return err
	}
	if strings.TrimSpace(actor) == "" {
		return errors.New("receive actor is required")
	}
	if handler == nil {
		return errors.New("receive handler is required")
	}
	receiveLock := c.receiveLock(repo)
	receiveLock.Lock()
	defer receiveLock.Unlock()

	// A previous reserved receive must be recovered before Ensure is allowed to
	// refresh public refs from upstream; otherwise unrelated upstream movement
	// could be misattributed to the prior actor's crash window.
	if err := c.replayRepoSerialized(ctx, repo, path, handler); err != nil {
		return fmt.Errorf("replay earlier receive for %s: %w", repo, err)
	}
	lock := c.repoLock(repo)
	lock.Lock()
	repository, err := c.ensureLocked(ctx, repo, path)
	lock.Unlock()
	if err != nil {
		return err
	}
	if repository.Stale && stderr != nil {
		_, _ = io.WriteString(stderr, "remote: oberth: upstream unavailable; serving cached repository\n")
	}
	return c.receiveTransactionSerialized(ctx, repo, path, actor, func() (ReleaseAdmission, error) {
		admission, err := c.prepareReleaseAdmissionLocked(ctx, path, !repository.Stale)
		if err != nil {
			return ReleaseAdmission{}, fmt.Errorf("prepare release admission: %w", err)
		}
		return admission, nil
	}, func() error {
		return c.serveLocked(ctx, path, ReceivePack, protocol, stdin, stdout, stderr)
	}, handler)
}

func (c *Cache) receiveTransaction(ctx context.Context, repo, path, actor string, receive func() error, handler ReceiveHandler) error {
	if strings.TrimSpace(actor) == "" {
		return errors.New("receive actor is required")
	}
	if receive == nil || handler == nil {
		return errors.New("receive operation and handler are required")
	}
	receiveLock := c.receiveLock(repo)
	receiveLock.Lock()
	defer receiveLock.Unlock()
	return c.receiveTransactionSerialized(ctx, repo, path, actor, nil, receive, handler)
}

func (c *Cache) receiveTransactionSerialized(
	ctx context.Context,
	repo, path, actor string,
	prepare func() (ReleaseAdmission, error),
	receive func() error,
	handler ReceiveHandler,
) error {
	if err := c.replayRepoSerialized(ctx, repo, path, handler); err != nil {
		return fmt.Errorf("replay earlier receive for %s: %w", repo, err)
	}

	lock := c.repoLock(repo)
	lock.Lock()
	if !c.isBare(ctx, path) {
		lock.Unlock()
		return fmt.Errorf("repository %s is not cached", repo)
	}
	if err := c.cleanReplacementRefsLocked(ctx, path); err != nil {
		lock.Unlock()
		return fmt.Errorf("clean replacement refs for %s: %w", repo, err)
	}
	if err := c.cleanInvalidPublicRefsLocked(ctx, path); err != nil {
		lock.Unlock()
		return fmt.Errorf("clean invalid public refs for %s: %w", repo, err)
	}
	admission := ReleaseAdmission{}
	if prepare != nil {
		prepared, err := prepare()
		if err != nil {
			lock.Unlock()
			return err
		}
		admission = prepared
	}
	reservation, err := c.reserveReceiveLocked(ctx, repo, path, actor, admission)
	if err != nil {
		lock.Unlock()
		return err
	}

	receiveErr := receive()
	finalizeErr := c.finalizeReceiveLocked(ctx, path, reservation)
	lock.Unlock()
	if finalizeErr != nil {
		return errors.Join(receiveErr, finalizeErr)
	}
	// Callback delivery stays under receiveLock for strict per-repository order,
	// but outside repoLock so handlers can safely inspect accepted Git objects.
	replayErr := c.replayRepoSerialized(ctx, repo, path, handler)
	return errors.Join(receiveErr, replayErr)
}

func (c *Cache) reserveReceiveLocked(ctx context.Context, repo, path, actor string, admission ReleaseAdmission) (receiveReservation, error) {
	before, err := c.listRefs(ctx, path, "refs/heads/", "refs/tags/")
	if err != nil {
		return receiveReservation{}, fmt.Errorf("snapshot refs before receive: %w", err)
	}
	id, err := newReceiveID()
	if err != nil {
		return receiveReservation{}, err
	}
	reservation := receiveReservation{
		Version:          receiveReservationVersion,
		ID:               id,
		Repo:             repo,
		Actor:            actor,
		State:            receiveReserved,
		Before:           before,
		ReleaseAdmission: admission,
	}
	if err := c.writeReservation(reservation); err != nil {
		return receiveReservation{}, fmt.Errorf("persist receive reservation: %w", err)
	}
	return reservation, nil
}

func (c *Cache) finalizeReceiveLocked(ctx context.Context, path string, reservation receiveReservation) error {
	after, err := c.listRefs(ctx, path, "refs/heads/", "refs/tags/")
	if err != nil {
		// The reserved record remains recoverable: startup replay compares its
		// before snapshot with the repository's current refs.
		return fmt.Errorf("snapshot refs after receive: %w", err)
	}
	reservation.State = receiveReady
	reservation.Updates = DiffRefs(reservation.Before, after)
	reservation.Next = 0
	if len(reservation.Updates) == 0 {
		return c.removeReservation(reservation.Repo)
	}
	if err := c.writeReservation(reservation); err != nil {
		return fmt.Errorf("finalize receive reservation: %w", err)
	}
	return nil
}

// ReplayPending recovers and delivers every durable receive reservation. Call
// this during process startup before opening the SSH listener. Receive also
// invokes the same replay for its repository before accepting a later push.
func (c *Cache) ReplayPending(ctx context.Context, handler ReceiveHandler) error {
	if handler == nil {
		return errors.New("receive replay handler is required")
	}
	pending, err := c.PendingReceives()
	if err != nil {
		return err
	}
	var replayErrs []error
	for _, item := range pending {
		repo, path, pathErr := c.path(item.Repo)
		if pathErr != nil {
			replayErrs = append(replayErrs, pathErr)
			continue
		}
		receiveLock := c.receiveLock(repo)
		receiveLock.Lock()
		err := c.replayRepoSerialized(ctx, repo, path, handler)
		receiveLock.Unlock()
		if err != nil {
			replayErrs = append(replayErrs, fmt.Errorf("replay receive for %s: %w", repo, err))
		}
	}
	return errors.Join(replayErrs...)
}

// PendingReceives returns durable outbox metadata without delivering events.
func (c *Cache) PendingReceives() ([]PendingReceive, error) {
	entries, err := os.ReadDir(c.outboxRoot)
	if err != nil {
		return nil, fmt.Errorf("list receive outbox: %w", err)
	}
	result := make([]PendingReceive, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		repo := strings.TrimSuffix(entry.Name(), ".json")
		reservation, err := c.readReservation(repo)
		if err != nil {
			return nil, err
		}
		remaining := len(reservation.Updates) - reservation.Next
		if remaining < 0 {
			remaining = 0
		}
		result = append(result, PendingReceive{ID: reservation.ID, Repo: repo, Actor: reservation.Actor, State: reservation.State, Remaining: remaining})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Repo < result[j].Repo })
	return result, nil
}

// replayRepoSerialized requires receiveLock for repo. It holds repoLock only
// while inspecting/finalizing Git state, then releases it before callbacks.
func (c *Cache) replayRepoSerialized(ctx context.Context, repo, path string, handler ReceiveHandler) error {
	reservation, err := c.readReservation(repo)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	lock := c.repoLock(repo)
	lock.Lock()
	if !c.isBare(ctx, path) {
		lock.Unlock()
		return fmt.Errorf("repository %s is not cached", repo)
	}
	if err := c.cleanReplacementRefsLocked(ctx, path); err != nil {
		lock.Unlock()
		return fmt.Errorf("clean replacement refs for %s: %w", repo, err)
	}
	if err := c.cleanInvalidPublicRefsLocked(ctx, path); err != nil {
		lock.Unlock()
		return fmt.Errorf("clean invalid public refs for %s: %w", repo, err)
	}
	if reservation.State == receiveReserved {
		if err := c.finalizeReceiveLocked(ctx, path, reservation); err != nil {
			lock.Unlock()
			return err
		}
		lock.Unlock()
		reservation, err = c.readReservation(repo)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
	} else {
		lock.Unlock()
	}
	for reservation.Next < len(reservation.Updates) {
		index := reservation.Next
		event := ReceiveEvent{
			ID:               fmt.Sprintf("%s:%d", reservation.ID, index),
			Actor:            reservation.Actor,
			Repo:             reservation.Repo,
			Update:           reservation.Updates[index],
			ReleaseAdmission: reservation.ReleaseAdmission,
		}
		if err := handler.HandleReceive(ctx, event); err != nil {
			return fmt.Errorf("deliver receive event %s: %w", event.ID, err)
		}
		reservation.Next++
		if reservation.Next == len(reservation.Updates) {
			return c.removeReservation(repo)
		}
		if err := c.writeReservation(reservation); err != nil {
			return fmt.Errorf("checkpoint receive event %s: %w", event.ID, err)
		}
	}
	return c.removeReservation(repo)
}

func (c *Cache) reservationPath(repo string) (string, error) {
	canonical, err := NormalizeRepo(repo)
	if err != nil {
		return "", err
	}
	path := filepath.Join(c.outboxRoot, canonical+".json")
	relative, err := filepath.Rel(c.outboxRoot, path)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", errors.New("receive outbox path escapes root")
	}
	return path, nil
}

func (c *Cache) readReservation(repo string) (receiveReservation, error) {
	path, err := c.reservationPath(repo)
	if err != nil {
		return receiveReservation{}, err
	}
	// reservationPath canonicalizes the repository name and proves containment
	// under the configured outbox root before this variable-path read.
	body, err := os.ReadFile(path) //nolint:gosec
	if err != nil {
		return receiveReservation{}, err
	}
	var reservation receiveReservation
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&reservation); err != nil {
		return receiveReservation{}, fmt.Errorf("decode receive reservation for %s: %w", repo, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return receiveReservation{}, fmt.Errorf("receive reservation for %s has trailing data", repo)
	}
	if err := validateReservation(reservation, repo); err != nil {
		return receiveReservation{}, err
	}
	return reservation, nil
}

func validateReservation(reservation receiveReservation, repo string) error {
	if reservation.Version != receiveReservationVersion || reservation.Repo != repo || reservation.ID == "" || strings.TrimSpace(reservation.Actor) == "" ||
		(reservation.State != receiveReserved && reservation.State != receiveReady) || reservation.Next < 0 || reservation.Next > len(reservation.Updates) {
		return fmt.Errorf("receive reservation for %s is invalid", repo)
	}
	for ref, sha := range reservation.Before {
		if !publicReceiveRef(ref) || ValidateSHA(sha) != nil {
			return fmt.Errorf("receive reservation for %s has invalid before ref", repo)
		}
	}
	if err := validateReleaseAdmission(reservation.ReleaseAdmission); err != nil {
		return fmt.Errorf("receive reservation for %s has invalid release admission: %w", repo, err)
	}
	for _, update := range reservation.Updates {
		if !publicReceiveRef(update.Ref) || (update.OldSHA != "" && ValidateSHA(update.OldSHA) != nil) || (update.NewSHA != "" && ValidateSHA(update.NewSHA) != nil) {
			return fmt.Errorf("receive reservation for %s has invalid update", repo)
		}
		if update.Kind() == "tag" && update.NewSHA != "" && reservation.ReleaseAdmission.SHA == "" {
			return fmt.Errorf("receive reservation for %s has a tag without release admission", repo)
		}
	}
	return nil
}

func publicReceiveRef(ref string) bool {
	switch {
	case strings.HasPrefix(ref, "refs/heads/"):
		return ValidateBranch(strings.TrimPrefix(ref, "refs/heads/")) == nil
	case strings.HasPrefix(ref, "refs/tags/"):
		return ValidateTag(strings.TrimPrefix(ref, "refs/tags/")) == nil
	default:
		return false
	}
}

func validateReleaseAdmission(admission ReleaseAdmission) error {
	if admission.DefaultBranch == "" && admission.SHA == "" {
		return nil
	}
	if err := ValidateBranch(admission.DefaultBranch); err != nil {
		return err
	}
	return ValidateSHA(admission.SHA)
}

func (c *Cache) writeReservation(reservation receiveReservation) error {
	if err := validateReservation(reservation, reservation.Repo); err != nil {
		return err
	}
	path, err := c.reservationPath(reservation.Repo)
	if err != nil {
		return err
	}
	body, err := json.Marshal(reservation)
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(c.outboxRoot, ".receive-*.tmp")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	clean := false
	defer func() {
		_ = temporary.Close()
		if !clean {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return err
	}
	if _, err := temporary.Write(body); err != nil {
		return err
	}
	if err := temporary.Sync(); err != nil {
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return err
	}
	clean = true
	return syncDirectory(c.outboxRoot)
}

func (c *Cache) removeReservation(repo string) error {
	path, err := c.reservationPath(repo)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return syncDirectory(c.outboxRoot)
}

func syncDirectory(path string) error {
	// Callers pass only the configured, canonical cache/outbox directories.
	directory, err := os.Open(path) //nolint:gosec
	if err != nil {
		return err
	}
	defer func() { _ = directory.Close() }()
	return directory.Sync()
}

func newReceiveID() (string, error) {
	var value [16]byte
	if _, err := cryptorand.Read(value[:]); err != nil {
		return "", fmt.Errorf("generate receive reservation id: %w", err)
	}
	return hex.EncodeToString(value[:]), nil
}
