package auditanchor

import (
	"bytes"
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
)

type AnchorStore interface {
	VerifyAuditState(context.Context) (model.AuditHead, error)
	AuditHeadHint(context.Context) (model.AuditHead, error)
	VerifyAuditMutationState(context.Context, []model.AuditWitnessIntent, []model.AuditWitness, func(model.AuditHead, model.AuditAnchor) error) (model.AuditHead, model.AuditAnchor, error)
	LatestAuditAnchor(context.Context) (model.AuditAnchor, error)
	RecordAuditAnchor(context.Context, model.AuditAnchorSpec) (model.AuditAnchor, error)
	VerifyAuditAnchorReferences(context.Context, []model.AuditAnchor) error
	VerifyAuditWitnesses(context.Context, []model.AuditWitness) error
	VerifyAuditWitnessIntents(context.Context, []model.AuditWitnessIntent) error
}

type Authority interface {
	Timestamp(context.Context, model.AuditHead) (model.AuditAnchorSpec, error)
	Verify(model.AuditAnchor) error
}

type Witness interface {
	History(context.Context, []model.AuditWitness, model.AuditHead) ([]model.AuditWitness, error)
	Publish(context.Context, model.AuditHead) (model.AuditWitness, error)
}

// Continuity pins verified public witness history outside the SQLite/PVC
// rollback boundary. The production implementation uses immutable Kubernetes
// records that Oberth can create and read but cannot update or delete.
type Continuity interface {
	Pinned(context.Context) ([]model.AuditWitness, error)
	Intents(context.Context) ([]model.AuditWitnessIntent, error)
	Prepare(context.Context, model.AuditWitnessIntent) error
	Reconcile(context.Context, []model.AuditWitness) error
}

type managerLogger interface {
	Printf(string, ...any)
}

var ErrWitnessUnavailable = errors.New("audit anchor: external witness unavailable")

type ManagerConfig struct {
	Store      AnchorStore
	Authority  Authority
	Witness    Witness
	Continuity Continuity
	Interval   time.Duration
	MaxAge     time.Duration
	Now        func() time.Time
	Logger     managerLogger
}

type Manager struct {
	store      AnchorStore
	authority  Authority
	witness    Witness
	continuity Continuity
	interval   time.Duration
	maxAge     time.Duration
	now        func() time.Time
	logger     managerLogger

	refreshMu              sync.Mutex
	mu                     sync.RWMutex
	checked                bool
	witnessHistoryVerified bool
	anchor                 model.AuditAnchor
	err                    error
}

func NewManager(config ManagerConfig) (*Manager, error) {
	if config.Store == nil || config.Authority == nil || config.Witness == nil || config.Continuity == nil {
		return nil, errors.New("audit anchor: store, timestamp authority, external witness, and rollback-external continuity are required")
	}
	if config.Interval <= 0 || config.MaxAge < config.Interval {
		return nil, errors.New("audit anchor: positive interval and max age at least as long as the interval are required")
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	return &Manager{
		store: config.Store, authority: config.Authority, witness: config.Witness, continuity: config.Continuity, interval: config.Interval,
		maxAge: config.MaxAge, now: config.Now, logger: config.Logger,
	}, nil
}

func (manager *Manager) Run(ctx context.Context) error {
	delay := time.Duration(0)
	if manager.Ready(ctx) == nil {
		delay = manager.interval
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-timer.C:
			manager.cycle(ctx)
			delay := manager.interval
			if manager.Ready(ctx) != nil && delay > 10*time.Second {
				delay = 10 * time.Second
			}
			timer.Reset(delay)
		}
	}
}

// VerifyStartup performs the complete local, public-witness, and
// rollback-external continuity verification without creating an intent,
// reconciling a continuity object, or writing a local checkpoint. The daemon
// runs this against a read-only SQLite handle before opening an existing
// database writable.
func (manager *Manager) VerifyStartup(ctx context.Context) error {
	manager.refreshMu.Lock()
	defer manager.refreshMu.Unlock()

	head, err := manager.store.VerifyAuditState(ctx)
	if err != nil {
		return fmt.Errorf("audit anchor: startup verify local protected state: %w", err)
	}
	existing, err := manager.store.LatestAuditAnchor(ctx)
	hasExisting := err == nil
	if hasExisting {
		if err := manager.authority.Verify(existing); err != nil {
			return fmt.Errorf("audit anchor: startup verify signed checkpoint: %w", err)
		}
		if err := manager.store.VerifyAuditAnchorReferences(ctx, []model.AuditAnchor{existing}); err != nil {
			return fmt.Errorf("audit anchor: startup verify checkpoint reference: %w", err)
		}
	} else if !errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("audit anchor: startup read latest local checkpoint: %w", err)
	}

	history, pending, err := manager.recoverWitnessHistory(ctx, head)
	if err != nil {
		return fmt.Errorf("audit anchor: startup recover external witness history: %w", err)
	}
	if hasExisting {
		if len(history) == 0 {
			return errors.New("audit anchor: local checkpoints exist without recoverable external witness history")
		}
		anchoredHead := model.AuditHead{ID: existing.AuditID, SHA256: append([]byte(nil), existing.AuditSHA256...)}
		if !historyIncludesHead(history, anchoredHead) {
			return errors.New("audit anchor: latest local checkpoint has no matching public witness")
		}
	} else if head.ID != 0 {
		pendingBindsHead := pending != nil && pending.AuditID == head.ID &&
			subtle.ConstantTimeCompare(pending.AuditSHA256, head.SHA256) == 1
		pendingFirstCheckpoint := len(history) == 0 && pendingBindsHead
		completedFirstCheckpoint := pending == nil && len(history) == 1 && historyIncludesHead(history, head)
		if !pendingFirstCheckpoint && !completedFirstCheckpoint {
			return errors.New("audit anchor: non-genesis audit history exists without a signed checkpoint or exact rollback-external witness")
		}
	}
	manager.markWitnessHistoryVerified()
	return nil
}

// Initialize completes recovery and records a fresh verified checkpoint before
// any network listener or application mutation path is exposed.
func (manager *Manager) Initialize(ctx context.Context) error {
	manager.cycle(ctx)
	return manager.Ready(ctx)
}

func (manager *Manager) Ready(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	manager.mu.RLock()
	checked, anchor, checkErr := manager.checked, manager.anchor, manager.err
	manager.mu.RUnlock()
	if !checked {
		return errors.New("audit anchor: initial checkpoint has not completed")
	}
	if checkErr != nil {
		return checkErr
	}
	age := manager.now().UTC().Sub(anchor.AnchoredAt)
	if age < 0 {
		return errors.New("audit anchor: latest checkpoint is in the future")
	}
	if age > manager.maxAge {
		return fmt.Errorf("audit anchor: latest checkpoint is stale by %s", age-manager.maxAge)
	}
	return nil
}

// AllowMutation rechecks the locally mutable state against the externally
// recovered witness set immediately before a caller performs a mutation. Ready
// alone is intentionally insufficient: it is a cached liveness signal and
// would leave a window between periodic anchor cycles.
func (manager *Manager) AllowMutation(ctx context.Context) error {
	manager.refreshMu.Lock()
	defer manager.refreshMu.Unlock()
	if err := manager.Ready(ctx); err != nil {
		return err
	}
	headHint, err := manager.store.AuditHeadHint(ctx)
	if err != nil {
		return fmt.Errorf("audit anchor: mutation gate head hint: %w", err)
	}
	intents, err := manager.continuity.Intents(ctx)
	if err != nil {
		return fmt.Errorf("audit anchor: mutation gate refresh external witness history: read rollback-external witness intents: %w", err)
	}
	history, pending, err := manager.loadUnverifiedWitnessHistoryForIntents(ctx, headHint, intents)
	if err != nil {
		return fmt.Errorf("audit anchor: mutation gate refresh external witness history: %w", err)
	}
	manager.mu.RLock()
	cachedAnchor := manager.anchor
	manager.mu.RUnlock()
	var gateErr error
	_, _, err = manager.store.VerifyAuditMutationState(ctx, intents, history, func(_ model.AuditHead, persisted model.AuditAnchor) error {
		if err := manager.authority.Verify(persisted); err != nil {
			gateErr = fmt.Errorf("audit anchor: mutation gate persisted checkpoints: %w", err)
			return gateErr
		}
		if !sameAuditAnchor(persisted, cachedAnchor) {
			gateErr = errors.New("audit anchor: mutation gate persisted checkpoint tip differs from the verified in-process tip")
			return gateErr
		}
		if err := manager.continuity.Reconcile(ctx, history); err != nil {
			gateErr = fmt.Errorf("audit anchor: mutation gate refresh external witness history: reconcile durable external witness history: %w", err)
			return gateErr
		}
		if pending != nil {
			gateErr = fmt.Errorf("audit anchor: mutation gate blocked by unresolved immutable witness intent %d", pending.Sequence)
			return gateErr
		}
		return nil
	})
	if err != nil {
		if gateErr != nil {
			return gateErr
		}
		return fmt.Errorf("audit anchor: mutation gate persisted checkpoints: %w", err)
	}
	manager.markWitnessHistoryVerified()
	return nil
}

func sameAuditAnchor(left, right model.AuditAnchor) bool {
	return left.ID == right.ID && left.AuditID == right.AuditID && bytes.Equal(left.AuditSHA256, right.AuditSHA256) &&
		left.TSAURL == right.TSAURL && bytes.Equal(left.Receipt, right.Receipt) && left.AnchoredAt.Equal(right.AnchoredAt) &&
		left.CreatedAt.Equal(right.CreatedAt)
}

func (manager *Manager) cycle(ctx context.Context) {
	manager.refreshMu.Lock()
	defer manager.refreshMu.Unlock()

	head, err := manager.store.VerifyAuditState(ctx)
	if err != nil {
		manager.setResult(model.AuditAnchor{}, fmt.Errorf("audit anchor: verify local protected state: %w", err))
		return
	}
	var existing model.AuditAnchor
	hasExisting := false
	existing, err = manager.store.LatestAuditAnchor(ctx)
	if err == nil {
		hasExisting = true
		if err := manager.authority.Verify(existing); err != nil {
			manager.setResult(model.AuditAnchor{}, err)
			return
		}
		if err := manager.store.VerifyAuditAnchorReferences(ctx, []model.AuditAnchor{existing}); err != nil {
			manager.setResult(model.AuditAnchor{}, fmt.Errorf("audit anchor: verify checkpoint reference: %w", err))
			return
		}
	} else if !errors.Is(err, store.ErrNotFound) {
		manager.setResult(model.AuditAnchor{}, fmt.Errorf("audit anchor: read latest local checkpoint: %w", err))
		return
	}

	history, pending, err := manager.recoverAndPinWitnessHistory(ctx, head)
	if err != nil {
		manager.finishRefreshFailure(existing, fmt.Errorf("audit anchor: recover external witness history: %w", err), errors.Is(err, ErrWitnessUnavailable))
		return
	}
	if hasExisting && len(history) == 0 {
		manager.setResult(model.AuditAnchor{}, errors.New("audit anchor: local checkpoints exist without recoverable external witness history"))
		return
	}
	manager.markWitnessHistoryVerified()

	checkpointHead := head
	if pending != nil {
		// A mutation that passed its gate before Prepare may finish after the
		// intent becomes visible. Complete that exact witnessed prefix first;
		// VerifyAuditWitnessIntents already proved it remains in the local chain.
		checkpointHead = model.AuditHead{ID: pending.AuditID, SHA256: append([]byte(nil), pending.AuditSHA256...)}
	}
	spec, err := manager.authority.Timestamp(ctx, checkpointHead)
	if err != nil {
		manager.finishRefreshFailure(existing, fmt.Errorf("audit anchor: create signed timestamp: %w", err), errors.Is(err, ErrAuthorityUnavailable))
		return
	}
	var expectedIntent *model.AuditWitnessIntent
	if !historyContainsHead(history, checkpointHead) {
		previousUUID := ""
		if len(history) > 0 {
			previousUUID = history[len(history)-1].UUID
		}
		if pending != nil {
			expectedIntent = pending
		} else {
			intent := model.AuditWitnessIntent{
				Sequence: len(history) + 1, AuditID: checkpointHead.ID,
				AuditSHA256: append([]byte(nil), checkpointHead.SHA256...), PreviousUUID: previousUUID,
			}
			if err := manager.continuity.Prepare(ctx, intent); err != nil {
				manager.setResult(model.AuditAnchor{}, fmt.Errorf("audit anchor: persist immutable witness intent before publication: %w", err))
				return
			}
			expectedIntent = &intent
		}
	}
	witness, err := manager.witness.Publish(ctx, checkpointHead)
	if err != nil {
		manager.finishRefreshFailure(existing, fmt.Errorf("audit anchor: publish external witness: %w", err), errors.Is(err, ErrWitnessUnavailable))
		return
	}
	if subtle.ConstantTimeCompare(witness.AuditSHA256, checkpointHead.SHA256) != 1 {
		manager.setResult(model.AuditAnchor{}, errors.New("audit anchor: published witness does not commit the current chain head"))
		return
	}
	if expectedIntent != nil && !witnessCompletesIntent(witness, *expectedIntent) {
		manager.setResult(model.AuditAnchor{}, fmt.Errorf(
			"audit anchor: published witness does not complete immutable intent %d", expectedIntent.Sequence,
		))
		return
	}
	if !containsWitness(history, witness.UUID) {
		history = append(history, witness)
	}
	if err := manager.verifyAndPinWitnessHistory(ctx, history); err != nil {
		manager.setResult(model.AuditAnchor{}, fmt.Errorf("audit anchor: persist published external witness: %w", err))
		return
	}
	latest, err := manager.store.RecordAuditAnchor(ctx, spec)
	if err != nil {
		manager.setResult(model.AuditAnchor{}, fmt.Errorf("audit anchor: record signed timestamp: %w", err))
		return
	}
	if err := manager.authority.Verify(latest); err != nil {
		manager.setResult(model.AuditAnchor{}, err)
		return
	}
	if err := manager.store.VerifyAuditAnchorReferences(ctx, []model.AuditAnchor{latest}); err != nil {
		manager.setResult(model.AuditAnchor{}, fmt.Errorf("audit anchor: verify checkpoint reference: %w", err))
		return
	}
	manager.setResult(latest, nil)
}

func (manager *Manager) recoverAndPinWitnessHistory(
	ctx context.Context,
	head model.AuditHead,
) ([]model.AuditWitness, *model.AuditWitnessIntent, error) {
	history, pending, err := manager.recoverWitnessHistory(ctx, head)
	if err != nil {
		return nil, nil, err
	}
	if err := manager.continuity.Reconcile(ctx, history); err != nil {
		return nil, nil, fmt.Errorf("reconcile durable external witness history: %w", err)
	}
	return history, pending, nil
}

func (manager *Manager) recoverWitnessHistory(
	ctx context.Context,
	head model.AuditHead,
) ([]model.AuditWitness, *model.AuditWitnessIntent, error) {
	intents, err := manager.continuity.Intents(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("read rollback-external witness intents: %w", err)
	}
	if err := manager.store.VerifyAuditWitnessIntents(ctx, intents); err != nil {
		return nil, nil, fmt.Errorf("verify rollback-external witness intents: %w", err)
	}
	history, pending, err := manager.loadUnverifiedWitnessHistoryForIntents(ctx, head, intents)
	if err != nil {
		return nil, nil, err
	}
	if err := manager.store.VerifyAuditWitnesses(ctx, history); err != nil {
		return nil, nil, fmt.Errorf("verify external witness history: %w", err)
	}
	return history, pending, nil
}

// loadUnverifiedWitnessHistoryForIntents reads public witness evidence but does
// not prove its local references. Callers must complete Store verification
// before any side effect.
func (manager *Manager) loadUnverifiedWitnessHistoryForIntents(
	ctx context.Context,
	head model.AuditHead,
	intents []model.AuditWitnessIntent,
) ([]model.AuditWitness, *model.AuditWitnessIntent, error) {
	pinned, err := manager.continuity.Pinned(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("read rollback-external continuity: %w", err)
	}
	if len(pinned) > len(intents) || len(intents) > len(pinned)+1 {
		return nil, nil, fmt.Errorf("audit anchor: immutable witness intent/completion counts differ: %d/%d", len(intents), len(pinned))
	}
	recoveryHead := head
	if len(intents) == len(pinned)+1 {
		pending := intents[len(intents)-1]
		recoveryHead = model.AuditHead{ID: pending.AuditID, SHA256: append([]byte(nil), pending.AuditSHA256...)}
	}
	history, err := manager.witness.History(ctx, pinned, recoveryHead)
	if err != nil {
		return nil, nil, err
	}
	if len(history) < len(pinned) {
		return nil, nil, errors.New("audit anchor: Rekor history omitted a rollback-external completed witness")
	}
	if len(history) > len(intents) {
		return nil, nil, errors.New("audit anchor: Rekor history contains a witness without a rollback-external publication intent")
	}
	for index, entry := range history {
		if intents[index].Sequence != index+1 || !witnessCompletesIntent(entry, intents[index]) {
			return nil, nil, fmt.Errorf("audit anchor: immutable witness intent %d differs from its completed Rekor record", index+1)
		}
	}
	if len(history) == len(intents) {
		return history, nil, nil
	}
	pending := cloneAuditWitnessIntent(intents[len(intents)-1])
	return history, &pending, nil
}

func witnessCompletesIntent(witness model.AuditWitness, intent model.AuditWitnessIntent) bool {
	return witness.AuditID == intent.AuditID && witness.PreviousUUID == intent.PreviousUUID &&
		subtle.ConstantTimeCompare(witness.AuditSHA256, intent.AuditSHA256) == 1
}

func historyContainsHead(history []model.AuditWitness, head model.AuditHead) bool {
	if len(history) == 0 {
		return false
	}
	latest := history[len(history)-1]
	return latest.AuditID == head.ID && subtle.ConstantTimeCompare(latest.AuditSHA256, head.SHA256) == 1
}

func historyIncludesHead(history []model.AuditWitness, head model.AuditHead) bool {
	for _, witness := range history {
		if witness.AuditID == head.ID && subtle.ConstantTimeCompare(witness.AuditSHA256, head.SHA256) == 1 {
			return true
		}
	}
	return false
}

func (manager *Manager) verifyAndPinWitnessHistory(ctx context.Context, history []model.AuditWitness) error {
	if err := manager.store.VerifyAuditWitnesses(ctx, history); err != nil {
		return fmt.Errorf("verify external witness history: %w", err)
	}
	if err := manager.continuity.Reconcile(ctx, history); err != nil {
		return fmt.Errorf("reconcile durable external witness history: %w", err)
	}
	return nil
}

func (manager *Manager) markWitnessHistoryVerified() {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	manager.witnessHistoryVerified = true
}

func containsWitness(history []model.AuditWitness, uuid string) bool {
	for _, witness := range history {
		if witness.UUID == uuid {
			return true
		}
	}
	return false
}

func (manager *Manager) witnessedInThisProcess() bool {
	manager.mu.RLock()
	defer manager.mu.RUnlock()
	return manager.witnessHistoryVerified
}

func (manager *Manager) finishRefreshFailure(existing model.AuditAnchor, refreshErr error, allowFallback bool) {
	age := manager.now().UTC().Sub(existing.AnchoredAt)
	if allowFallback && manager.witnessedInThisProcess() && len(existing.Receipt) > 0 && age >= 0 && age <= manager.maxAge {
		manager.setResult(existing, nil)
		if manager.logger != nil {
			manager.logger.Printf("audit anchor refresh failed; externally verified checkpoint remains valid: %v", refreshErr)
		}
		return
	}
	manager.setResult(model.AuditAnchor{}, refreshErr)
}

func (manager *Manager) setResult(anchor model.AuditAnchor, err error) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	manager.checked = true
	manager.anchor = anchor
	manager.err = err
	if err != nil && manager.logger != nil {
		manager.logger.Printf("audit integrity unavailable: %v", err)
	}
}
