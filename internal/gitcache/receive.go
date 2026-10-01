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
	receiveReserved   = "reserved"
	receiveReady      = "ready"
	receiveGateFailed = "gate-failed"
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
	// The reservation identity is the canonical qualified spelling derived
	// from the resolved cache path, so same-named repositories under
	// different upstreams keep strictly disjoint durable reservations
	// (issue #264). For a flat (legacy) cache it degrades to the bare name,
	// which keeps pre-migration reservation files readable.
	identity := c.reservationIdentity(repo, path)
	if strings.TrimSpace(actor) == "" {
		return errors.New("receive actor is required")
	}
	if handler == nil {
		return errors.New("receive handler is required")
	}
	receiveLock := c.receiveLock(path)
	receiveLock.Lock()
	defer receiveLock.Unlock()

	// A previous reserved receive must be recovered before Ensure is allowed to
	// refresh public refs from upstream; otherwise unrelated upstream movement
	// could be misattributed to the prior actor's crash window.
	if err := c.replayRepoSerialized(ctx, identity, path, handler); err != nil {
		return fmt.Errorf("replay earlier receive for %s: %w", identity, err)
	}
	lock := c.repoLock(path)
	lock.Lock()
	repository, err := c.ensureReceiveLocked(ctx, input, repo, path)
	lock.Unlock()
	if err != nil {
		return err
	}
	if repository.recoveryOnly && stderr != nil {
		_, _ = fmt.Fprintf(stderr,
			"remote: oberth: repository exceeds %d public refs; only exact-current branch deletions that restore the limit are accepted\n",
			maximumPublicRefs,
		)
	} else if repository.Stale && stderr != nil {
		_, _ = io.WriteString(stderr, "remote: oberth: upstream unavailable; serving cached repository\n")
	}
	return c.receiveTransactionSerialized(ctx, identity, path, actor, repository.recoveryOnly, func() (ReleaseAdmission, error) {
		admission, err := c.prepareReleaseAdmissionLocked(ctx, path, !repository.Stale && !repository.recoveryOnly)
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
	identity := c.reservationIdentity(repo, path)
	receiveLock := c.receiveLock(path)
	receiveLock.Lock()
	defer receiveLock.Unlock()
	return c.receiveTransactionSerialized(ctx, identity, path, actor, false, nil, receive, handler)
}

// receiveTransactionSerialized requires receiveLock for the repository's
// path. `identity` is the reservation identity from reservationIdentity —
// the canonical qualified spelling for a qualified cache, the bare name for
// a legacy flat one.
func (c *Cache) receiveTransactionSerialized(
	ctx context.Context,
	identity, path, actor string,
	recoveryOnly bool,
	prepare func() (ReleaseAdmission, error),
	receive func() error,
	handler ReceiveHandler,
) error {
	if err := c.replayRepoSerialized(ctx, identity, path, handler); err != nil {
		return fmt.Errorf("replay earlier receive for %s: %w", identity, err)
	}

	lock := c.repoLock(path)
	lock.Lock()
	if !c.isBare(ctx, path) {
		lock.Unlock()
		return fmt.Errorf("repository %s is not cached", identity)
	}
	if err := c.cleanReplacementRefsLocked(ctx, path); err != nil {
		lock.Unlock()
		return fmt.Errorf("clean replacement refs for %s: %w", identity, err)
	}
	if _, err := c.cleanInvalidPublicRefsLocked(ctx, path); err != nil {
		lock.Unlock()
		return fmt.Errorf("clean invalid public refs for %s: %w", identity, err)
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
	refScanLimit := maximumPublicRefs
	if recoveryOnly {
		refScanLimit = maximumReceiveSnapshotRefs
	}
	reservation, err := c.reserveReceiveAtMostLocked(ctx, identity, path, actor, admission, refScanLimit)
	if err != nil {
		lock.Unlock()
		return err
	}

	receiveErr := receive()
	// The server-owned pre-finalize gate runs after receive-pack has applied
	// ref updates and before finalization records the delta in the durable
	// outbox. A gate failure (e.g. audit chain tampered during the pack
	// upload window) prevents finalization and callback delivery. The
	// reservation is kept in a gate-failed state preserving the original
	// actor, before-snapshot, and admission so that replay can re-run the
	// gate once it recovers. Removing the reservation on gate failure (the
	// prior #28-8 round-2 fix) caused record amnesia: the next receive
	// re-baselined the moved refs and lost the push permanently.
	// Already-finalized (receiveReady) replayed reservations bypass this
	// gate — their events are committed; the per-callback recheck in the
	// handler still applies to those.
	if receiveErr == nil && c.preFinalizeGate != nil {
		if gateErr := c.preFinalizeGate(ctx); gateErr != nil {
			reservation.State = receiveGateFailed
			writeErr := c.writeReservation(reservation)
			lock.Unlock()
			if writeErr != nil {
				return errors.Join(fmt.Errorf("pre-finalize gate: %w", gateErr), writeErr)
			}
			return fmt.Errorf("pre-finalize gate: %w", gateErr)
		}
	}
	finalizeErr := c.finalizeReceiveLocked(ctx, path, reservation)
	lock.Unlock()
	if finalizeErr != nil {
		return errors.Join(receiveErr, finalizeErr)
	}
	// Callback delivery stays under receiveLock for strict per-repository order,
	// but outside repoLock so handlers can safely inspect accepted Git objects.
	replayErr := c.replayRepoSerialized(ctx, identity, path, handler)
	return errors.Join(receiveErr, replayErr)
}

func (c *Cache) reserveReceiveLocked(ctx context.Context, identity, path, actor string, admission ReleaseAdmission) (receiveReservation, error) {
	return c.reserveReceiveAtMostLocked(ctx, identity, path, actor, admission, maximumPublicRefs)
}

func (c *Cache) reserveReceiveAtMostLocked(ctx context.Context, identity, path, actor string, admission ReleaseAdmission, maximum int) (receiveReservation, error) {
	before, err := c.listRefsAtMost(ctx, path, maximum, "refs/heads/", "refs/tags/")
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
		Repo:             identity,
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
	maximumAfter := maximumPublicRefs
	if len(reservation.Before) > maximumAfter {
		maximumAfter = len(reservation.Before)
	}
	after, err := c.listRefsAtMost(ctx, path, maximumAfter, "refs/heads/", "refs/tags/")
	if err != nil {
		// The reserved record remains recoverable: startup replay compares its
		// before snapshot with the repository's current refs.
		return fmt.Errorf("snapshot refs after receive: %w", err)
	}
	updates := DiffRefs(reservation.Before, after)
	// Bound the number of changes one receive can produce. The pre-receive
	// hook enforces this in the Git layer; this is a defense-in-depth check.
	if len(updates) > maximumReceiveRefUpdates {
		// Otherwise expand callback replay beyond the accepted bound.
		return fmt.Errorf("receive changed %d public refs; limit is %d", len(updates), maximumReceiveRefUpdates)
	}
	// Recovery receives (before snapshot > cap) may only contain exact-current
	// branch deletions. Non-deletion changes in a recovery indicate a hook
	// bypass or corrupted push and must be rejected.
	if len(reservation.Before) > maximumPublicRefs && len(updates) > 0 {
		if len(after) > maximumPublicRefs {
			return fmt.Errorf("public-ref recovery left %d public refs; limit is %d", len(after), maximumPublicRefs)
		}
		for _, update := range updates {
			if update.Kind() != "branch" || !update.Deleted() {
				return fmt.Errorf("public-ref recovery changed non-deletion ref %s", update.Ref)
			}
		}
	}
	reservation.State = receiveReady
	reservation.Updates = updates
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
		// item.Repo is the reservation identity (a valid repository input:
		// bare for legacy flat reservations, upstream/org/repo for qualified
		// ones), so path resolution and replay address exactly one cache.
		_, path, pathErr := c.path(item.Repo)
		if pathErr != nil {
			replayErrs = append(replayErrs, pathErr)
			continue
		}
		receiveLock := c.receiveLock(path)
		receiveLock.Lock()
		err := c.replayRepoSerialized(ctx, item.Repo, path, handler)
		receiveLock.Unlock()
		if err != nil {
			replayErrs = append(replayErrs, fmt.Errorf("replay receive for %s: %w", item.Repo, err))
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
		// Filenames encode the identity with "@" (a character no valid path
		// segment may contain) in place of "/"; a legacy bare-name file has
		// no "@" and round-trips unchanged.
		identity := strings.ReplaceAll(strings.TrimSuffix(entry.Name(), ".json"), "@", "/")
		reservation, err := c.readReservation(identity)
		if err != nil {
			return nil, err
		}
		remaining := len(reservation.Updates) - reservation.Next
		if remaining < 0 {
			remaining = 0
		}
		result = append(result, PendingReceive{ID: reservation.ID, Repo: identity, Actor: reservation.Actor, State: reservation.State, Remaining: remaining})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Repo < result[j].Repo })
	return result, nil
}

// replayRepoSerialized requires receiveLock for the repository's path. It
// holds repoLock only while inspecting/finalizing Git state, then releases
// it before callbacks. `identity` is the reservation identity (see
// reservationIdentity).
func (c *Cache) replayRepoSerialized(ctx context.Context, identity, path string, handler ReceiveHandler) error {
	reservation, err := c.readReservation(identity)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	lock := c.repoLock(path)
	lock.Lock()
	if !c.isBare(ctx, path) {
		lock.Unlock()
		return fmt.Errorf("repository %s is not cached", identity)
	}
	if err := c.cleanReplacementRefsLocked(ctx, path); err != nil {
		lock.Unlock()
		return fmt.Errorf("clean replacement refs for %s: %w", identity, err)
	}
	if _, err := c.cleanInvalidPublicRefsLocked(ctx, path); err != nil {
		lock.Unlock()
		return fmt.Errorf("clean invalid public refs for %s: %w", identity, err)
	}
	switch reservation.State {
	case receiveReserved, receiveGateFailed:
		// No reservation finalizes without a gate pass in the same or a
		// later session; only already-finalized (receiveReady) reservations
		// bypass. receiveReserved means the process crashed between
		// receive-pack and the gate call; receiveGateFailed means the gate
		// explicitly rejected. Both re-run the gate before finalization to
		// close the gate-bypass and record-amnesia windows (#28-8).
		if c.preFinalizeGate != nil {
			if gateErr := c.preFinalizeGate(ctx); gateErr != nil {
				lock.Unlock()
				return fmt.Errorf("replay pre-finalize gate for %s: %w", identity, gateErr)
			}
		}
		// Gate passed (or no longer configured) — finalize with original
		// actor attribution.
		if err := c.finalizeReceiveLocked(ctx, path, reservation); err != nil {
			lock.Unlock()
			return err
		}
		lock.Unlock()
		reservation, err = c.readReservation(identity)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
	default:
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
			return c.removeReservation(identity)
		}
		if err := c.writeReservation(reservation); err != nil {
			return fmt.Errorf("checkpoint receive event %s: %w", event.ID, err)
		}
	}
	return c.removeReservation(identity)
}

// reservationIdentity derives the durable reservation identity for a
// repository from its resolved cache path: the canonical qualified spelling
// ("upstream/org/repo") for a qualified cache directory, or the bare name
// for a legacy flat one. Deriving from the path — never from the client
// input — makes every spelling of one repository share one reservation and
// keeps same-named repositories under different upstreams disjoint
// (issue #264).
func (c *Cache) reservationIdentity(repo, cachePath string) string {
	relative, err := filepath.Rel(c.root, cachePath)
	if err != nil {
		return repo
	}
	relative = strings.TrimSuffix(filepath.ToSlash(relative), ".git")
	if relative == "" || strings.HasPrefix(relative, "../") || relative == ".." {
		return repo
	}
	return relative
}

// reservationPath maps a reservation identity to its durable outbox file.
// Each "/"-separated segment is canonicalized individually and the filename
// joins them with "@" — a character the segment charset forbids — so a
// qualified identity can never collide with a legacy bare-name file and two
// same-named repositories under different upstreams keep disjoint files
// (issue #264).
func (c *Cache) reservationPath(identity string) (string, error) {
	segments := strings.Split(identity, "/")
	canonicalSegments := make([]string, 0, len(segments))
	for _, segment := range segments {
		canonicalSegment, err := NormalizeRepo(segment)
		if err != nil {
			return "", err
		}
		canonicalSegments = append(canonicalSegments, canonicalSegment)
	}
	canonical := strings.Join(canonicalSegments, "@")
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
		(reservation.State != receiveReserved && reservation.State != receiveReady && reservation.State != receiveGateFailed) || reservation.Next < 0 || reservation.Next > len(reservation.Updates) ||
		len(reservation.Before) > maximumReceiveSnapshotRefs || len(reservation.Updates) > maximumReceiveRefUpdates {
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
	if len(reservation.Before) > maximumPublicRefs && reservation.ReleaseAdmission.SHA != "" {
		return fmt.Errorf("receive reservation for %s has release admission during public-ref recovery", repo)
	}
	for _, update := range reservation.Updates {
		if !publicReceiveRef(update.Ref) || (update.OldSHA != "" && ValidateSHA(update.OldSHA) != nil) || (update.NewSHA != "" && ValidateSHA(update.NewSHA) != nil) {
			return fmt.Errorf("receive reservation for %s has invalid update", repo)
		}
		if update.Kind() == "tag" && update.NewSHA != "" && reservation.ReleaseAdmission.SHA == "" {
			return fmt.Errorf("receive reservation for %s has a tag without release admission", repo)
		}
		if len(reservation.Before) > maximumPublicRefs && (update.Kind() != "branch" || !update.Deleted()) {
			return fmt.Errorf("receive reservation for %s has a non-deletion recovery update", repo)
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
