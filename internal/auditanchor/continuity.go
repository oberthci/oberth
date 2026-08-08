package auditanchor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/oberthci/oberth/internal/model"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const (
	continuityLabelKey            = "oberth.ci/audit-witness"
	continuityLabelValue          = "continuity"
	continuityIntentLabelValue    = "intent"
	continuityManagedByLabelKey   = "app.kubernetes.io/managed-by"
	continuityManagedByLabelValue = "oberth"
	continuityNamePrefix          = "oberth-audit-witness-"
	continuityIntentNamePrefix    = "oberth-audit-witness-intent-"
	maximumWitnessUUIDBytes       = 512
	continuityListPageSize        = 500
	continuityCreateReadbackLimit = 15 * time.Second
)

type KubernetesContinuity struct {
	client    kubernetes.Interface
	namespace string

	mu            sync.Mutex
	pinnedLoaded  bool
	pinned        []model.AuditWitness
	intentsLoaded bool
	intents       []model.AuditWitnessIntent
}

type continuityRecord struct {
	Sequence     int    `json:"sequence"`
	UUID         string `json:"uuid"`
	LogIndex     int64  `json:"logIndex"`
	IntegratedAt string `json:"integratedAt"`
	AuditID      int64  `json:"auditID"`
	AuditSHA256  string `json:"auditSHA256"`
	PreviousUUID string `json:"previousUUID"`
}

type continuityIntentRecord struct {
	Sequence     int    `json:"sequence"`
	AuditID      int64  `json:"auditID"`
	AuditSHA256  string `json:"auditSHA256"`
	PreviousUUID string `json:"previousUUID"`
}

func NewKubernetesContinuity(client kubernetes.Interface, namespace string) (*KubernetesContinuity, error) {
	if client == nil || strings.TrimSpace(namespace) == "" || strings.ContainsAny(namespace, "\x00\r\n") {
		return nil, errors.New("audit anchor: Kubernetes client and namespace are required for rollback-external continuity")
	}
	return &KubernetesContinuity{client: client, namespace: namespace}, nil
}

// Pinned returns the complete, ordered witness sequence that has already
// crossed the SQLite/PVC rollback boundary. Every object is regenerated from
// its canonical payload before it is trusted; gaps and forks fail closed.
func (continuity *KubernetesContinuity) Pinned(ctx context.Context) ([]model.AuditWitness, error) {
	continuity.mu.Lock()
	defer continuity.mu.Unlock()
	history, err := continuity.loadPinned(ctx)
	if err != nil {
		return nil, err
	}
	if continuity.pinnedLoaded {
		if err := verifyCachedPinnedPrefix(continuity.pinned, history); err != nil {
			return nil, err
		}
	}
	continuity.pinned = cloneAuditWitnesses(history)
	continuity.pinnedLoaded = true
	return history, nil
}

func (continuity *KubernetesContinuity) loadPinned(ctx context.Context) ([]model.AuditWitness, error) {
	existing, err := continuity.list(ctx, continuityLabelValue)
	if err != nil {
		return nil, err
	}
	bySequence := make(map[int]model.AuditWitness, len(existing))
	for _, item := range existing {
		var record continuityRecord
		if err := json.Unmarshal([]byte(item.Data["record.json"]), &record); err != nil {
			return nil, fmt.Errorf("audit anchor: decode immutable continuity record %s: %w", item.Name, err)
		}
		digest, err := hex.DecodeString(record.AuditSHA256)
		if err != nil {
			return nil, fmt.Errorf("audit anchor: decode immutable continuity record %s audit hash: %w", item.Name, err)
		}
		integratedAt, err := time.Parse(time.RFC3339Nano, record.IntegratedAt)
		if err != nil {
			return nil, fmt.Errorf("audit anchor: decode immutable continuity record %s integration time: %w", item.Name, err)
		}
		witness := model.AuditWitness{
			UUID: record.UUID, LogIndex: record.LogIndex, IntegratedAt: integratedAt.UTC(), AuditID: record.AuditID,
			AuditSHA256: digest, PreviousUUID: record.PreviousUUID,
		}
		if record.Sequence <= 0 {
			return nil, fmt.Errorf("audit anchor: immutable continuity record %s has invalid sequence %d", item.Name, record.Sequence)
		}
		if _, duplicate := bySequence[record.Sequence]; duplicate {
			return nil, fmt.Errorf("audit anchor: duplicate immutable continuity sequence %d", record.Sequence)
		}
		expected, err := continuityConfigMap(continuity.namespace, record.Sequence, witness)
		if err != nil {
			return nil, err
		}
		if !matchingContinuityConfigMap(item, expected) {
			return nil, fmt.Errorf("audit anchor: immutable continuity record %s differs from its canonical payload", item.Name)
		}
		bySequence[record.Sequence] = witness
	}
	sequences := make([]int, 0, len(bySequence))
	for sequence := range bySequence {
		sequences = append(sequences, sequence)
	}
	sort.Ints(sequences)
	history := make([]model.AuditWitness, 0, len(sequences))
	for index, sequence := range sequences {
		if sequence != index+1 {
			return nil, fmt.Errorf("audit anchor: immutable continuity sequence has a gap before %d", sequence)
		}
		history = append(history, bySequence[sequence])
	}
	if err := validateContinuityHistory(history); err != nil {
		return nil, err
	}
	return history, nil
}

// Intents returns the immutable publication intents created before any Rekor
// side effect. A pending suffix therefore remains visible even if the process
// crashes and the complete SQLite/PVC state is subsequently rolled back.
func (continuity *KubernetesContinuity) Intents(ctx context.Context) ([]model.AuditWitnessIntent, error) {
	continuity.mu.Lock()
	defer continuity.mu.Unlock()
	intents, err := continuity.loadIntents(ctx)
	if err != nil {
		return nil, err
	}
	if continuity.intentsLoaded {
		if err := verifyCachedIntentPrefix(continuity.intents, intents); err != nil {
			return nil, err
		}
	}
	continuity.intents = cloneAuditWitnessIntents(intents)
	continuity.intentsLoaded = true
	return intents, nil
}

func (continuity *KubernetesContinuity) loadIntents(ctx context.Context) ([]model.AuditWitnessIntent, error) {
	existing, err := continuity.list(ctx, continuityIntentLabelValue)
	if err != nil {
		return nil, err
	}
	bySequence := make(map[int]model.AuditWitnessIntent, len(existing))
	for _, item := range existing {
		var record continuityIntentRecord
		if err := json.Unmarshal([]byte(item.Data["record.json"]), &record); err != nil {
			return nil, fmt.Errorf("audit anchor: decode immutable witness intent %s: %w", item.Name, err)
		}
		digest, err := hex.DecodeString(record.AuditSHA256)
		if err != nil {
			return nil, fmt.Errorf("audit anchor: decode immutable witness intent %s audit hash: %w", item.Name, err)
		}
		intent := model.AuditWitnessIntent{
			Sequence: record.Sequence, AuditID: record.AuditID, AuditSHA256: digest, PreviousUUID: record.PreviousUUID,
		}
		if err := validateContinuityIntent(intent); err != nil {
			return nil, err
		}
		if _, duplicate := bySequence[intent.Sequence]; duplicate {
			return nil, fmt.Errorf("audit anchor: duplicate immutable witness-intent sequence %d", intent.Sequence)
		}
		expected, err := continuityIntentConfigMap(continuity.namespace, intent)
		if err != nil {
			return nil, err
		}
		if !matchingContinuityConfigMap(item, expected) {
			return nil, fmt.Errorf("audit anchor: immutable witness intent %s differs from its canonical payload", item.Name)
		}
		bySequence[intent.Sequence] = intent
	}
	sequences := make([]int, 0, len(bySequence))
	for sequence := range bySequence {
		sequences = append(sequences, sequence)
	}
	sort.Ints(sequences)
	intents := make([]model.AuditWitnessIntent, 0, len(sequences))
	for index, sequence := range sequences {
		if sequence != index+1 {
			return nil, fmt.Errorf("audit anchor: immutable witness-intent sequence has a gap before %d", sequence)
		}
		intent := bySequence[sequence]
		if index > 0 && intent.AuditID <= intents[index-1].AuditID {
			return nil, fmt.Errorf("audit anchor: immutable witness intent %d does not advance the audit sequence", sequence)
		}
		intents = append(intents, intent)
	}
	return intents, nil
}

// Prepare creates the immutable rollback-external intent before Rekor is
// called. Ambiguous Kubernetes outcomes reconcile only by exact readback.
func (continuity *KubernetesContinuity) Prepare(ctx context.Context, intent model.AuditWitnessIntent) error {
	continuity.mu.Lock()
	defer continuity.mu.Unlock()
	record, err := continuityIntentConfigMap(continuity.namespace, intent)
	if err != nil {
		return err
	}
	created, createErr := continuity.client.CoreV1().ConfigMaps(continuity.namespace).Create(ctx, record, metav1.CreateOptions{})
	if createErr == nil {
		if !matchingContinuityConfigMap(created, record) {
			return fmt.Errorf("audit anchor: created witness intent %s differs from its exact immutable intent", record.Name)
		}
		return continuity.rememberIntentLocked(intent)
	}
	readbackCtx, cancel := continuityCreateReadbackContext(ctx)
	defer cancel()
	readback, getErr := continuity.client.CoreV1().ConfigMaps(continuity.namespace).Get(readbackCtx, record.Name, metav1.GetOptions{})
	if getErr != nil {
		return errors.Join(fmt.Errorf("audit anchor: create witness intent %s: %w", record.Name, createErr),
			fmt.Errorf("read back witness intent %s: %w", record.Name, getErr))
	}
	if !matchingContinuityConfigMap(readback, record) {
		return fmt.Errorf("audit anchor: witness intent %s exists with different immutable content after create failure", record.Name)
	}
	return continuity.rememberIntentLocked(intent)
}

// Reconcile proves that every immutable continuity record already outside the
// SQLite PVC is an exact prefix of the freshly verified Rekor history, creates
// only the missing suffix, and reads the complete set back. Oberth's Role has
// no ConfigMap update or delete verb, so replacing the database with an older
// snapshot cannot roll these pins back with it.
func (continuity *KubernetesContinuity) Reconcile(ctx context.Context, history []model.AuditWitness) error {
	continuity.mu.Lock()
	defer continuity.mu.Unlock()
	if err := validateContinuityHistory(history); err != nil {
		return err
	}
	expected := make(map[string]*corev1.ConfigMap, len(history))
	for index, witness := range history {
		record, err := continuityConfigMap(continuity.namespace, index+1, witness)
		if err != nil {
			return err
		}
		expected[record.Name] = record
	}
	existing := make(map[string]*corev1.ConfigMap, len(continuity.pinned))
	if continuity.pinnedLoaded {
		if err := continuity.verifyLatestPinned(ctx); err != nil {
			return err
		}
		for index, witness := range continuity.pinned {
			record, err := continuityConfigMap(continuity.namespace, index+1, witness)
			if err != nil {
				return err
			}
			existing[record.Name] = record
		}
	} else {
		var err error
		existing, err = continuity.list(ctx, continuityLabelValue)
		if err != nil {
			return err
		}
	}
	if err := verifyContinuitySet(existing, expected); err != nil {
		return err
	}
	for index := range history {
		record, err := continuityConfigMap(continuity.namespace, index+1, history[index])
		if err != nil {
			return err
		}
		if _, ok := existing[record.Name]; ok {
			continue
		}
		created, createErr := continuity.client.CoreV1().ConfigMaps(continuity.namespace).Create(ctx, record, metav1.CreateOptions{})
		if createErr == nil {
			if !matchingContinuityConfigMap(created, record) {
				return fmt.Errorf("audit anchor: created continuity record %s differs from its exact immutable intent", record.Name)
			}
			continue
		}
		// Create responses are ambiguous under transport loss. Only an exact GET
		// of the deterministic immutable object may reconcile the operation.
		readbackCtx, cancel := continuityCreateReadbackContext(ctx)
		readback, getErr := continuity.client.CoreV1().ConfigMaps(continuity.namespace).Get(readbackCtx, record.Name, metav1.GetOptions{})
		cancel()
		if getErr != nil {
			return errors.Join(fmt.Errorf("audit anchor: create continuity record %s: %w", record.Name, createErr),
				fmt.Errorf("read back continuity record %s: %w", record.Name, getErr))
		}
		if !matchingContinuityConfigMap(readback, record) {
			return fmt.Errorf("audit anchor: continuity record %s exists with different immutable content after create failure", record.Name)
		}
	}
	if len(history) > 0 {
		latest, err := continuityConfigMap(continuity.namespace, len(history), history[len(history)-1])
		if err != nil {
			return err
		}
		readback, err := continuity.client.CoreV1().ConfigMaps(continuity.namespace).Get(ctx, latest.Name, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("audit anchor: read back latest continuity record %s: %w", latest.Name, err)
		}
		if !matchingContinuityConfigMap(readback, latest) {
			return fmt.Errorf("audit anchor: latest continuity record %s differs from verified history", latest.Name)
		}
	}
	continuity.pinned = cloneAuditWitnesses(history)
	continuity.pinnedLoaded = true
	return nil
}

func (continuity *KubernetesContinuity) rememberIntentLocked(intent model.AuditWitnessIntent) error {
	if !continuity.intentsLoaded {
		return nil
	}
	switch {
	case intent.Sequence == len(continuity.intents)+1:
		continuity.intents = append(continuity.intents, cloneAuditWitnessIntent(intent))
		return nil
	case intent.Sequence > 0 && intent.Sequence <= len(continuity.intents):
		if !sameAuditWitnessIntent(continuity.intents[intent.Sequence-1], intent) {
			return fmt.Errorf("audit anchor: cached immutable witness intent %d differs from exact create intent", intent.Sequence)
		}
		return nil
	default:
		return fmt.Errorf("audit anchor: immutable witness intent sequence jumped from %d to %d", len(continuity.intents), intent.Sequence)
	}
}

func verifyCachedPinnedPrefix(cached, reloaded []model.AuditWitness) error {
	if len(reloaded) < len(cached) {
		return fmt.Errorf("audit anchor: complete continuity reload omitted %d previously verified record(s)", len(cached)-len(reloaded))
	}
	for index := range cached {
		if !sameAuditWitness(cached[index], reloaded[index]) {
			return fmt.Errorf("audit anchor: complete continuity reload changed previously verified record %d", index+1)
		}
	}
	return nil
}

func verifyCachedIntentPrefix(cached, reloaded []model.AuditWitnessIntent) error {
	if len(reloaded) < len(cached) {
		return fmt.Errorf("audit anchor: complete intent reload omitted %d previously verified record(s)", len(cached)-len(reloaded))
	}
	for index := range cached {
		if !sameAuditWitnessIntent(cached[index], reloaded[index]) {
			return fmt.Errorf("audit anchor: complete intent reload changed previously verified record %d", index+1)
		}
	}
	return nil
}

func continuityCreateReadbackContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), continuityCreateReadbackLimit)
}

func (continuity *KubernetesContinuity) verifyLatestPinned(ctx context.Context) error {
	if len(continuity.pinned) == 0 {
		return nil
	}
	expected, err := continuityConfigMap(continuity.namespace, len(continuity.pinned), continuity.pinned[len(continuity.pinned)-1])
	if err != nil {
		return err
	}
	actual, err := continuity.client.CoreV1().ConfigMaps(continuity.namespace).Get(ctx, expected.Name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("audit anchor: read latest cached continuity record %s: %w", expected.Name, err)
	}
	if !matchingContinuityConfigMap(actual, expected) {
		return fmt.Errorf("audit anchor: latest cached continuity record %s changed", expected.Name)
	}
	return nil
}

func cloneAuditWitnesses(history []model.AuditWitness) []model.AuditWitness {
	cloned := make([]model.AuditWitness, len(history))
	for index, witness := range history {
		cloned[index] = witness
		cloned[index].AuditSHA256 = append([]byte(nil), witness.AuditSHA256...)
	}
	return cloned
}

func cloneAuditWitnessIntent(intent model.AuditWitnessIntent) model.AuditWitnessIntent {
	intent.AuditSHA256 = append([]byte(nil), intent.AuditSHA256...)
	return intent
}

func cloneAuditWitnessIntents(intents []model.AuditWitnessIntent) []model.AuditWitnessIntent {
	cloned := make([]model.AuditWitnessIntent, len(intents))
	for index, intent := range intents {
		cloned[index] = cloneAuditWitnessIntent(intent)
	}
	return cloned
}

func sameAuditWitnessIntent(left, right model.AuditWitnessIntent) bool {
	return left.Sequence == right.Sequence && left.AuditID == right.AuditID &&
		left.PreviousUUID == right.PreviousUUID && bytes.Equal(left.AuditSHA256, right.AuditSHA256)
}

func (continuity *KubernetesContinuity) list(ctx context.Context, labelValue string) (map[string]*corev1.ConfigMap, error) {
	selector := continuityLabelKey + "=" + labelValue
	result := make(map[string]*corev1.ConfigMap)
	continueToken := ""
	seenTokens := make(map[string]struct{})
	for {
		list, err := continuity.client.CoreV1().ConfigMaps(continuity.namespace).List(ctx, metav1.ListOptions{
			LabelSelector: selector, Limit: continuityListPageSize, Continue: continueToken,
		})
		if err != nil {
			return nil, fmt.Errorf("audit anchor: list rollback-external continuity records: %w", err)
		}
		for index := range list.Items {
			item := list.Items[index].DeepCopy()
			if _, duplicate := result[item.Name]; duplicate {
				return nil, fmt.Errorf("audit anchor: duplicate continuity record %s", item.Name)
			}
			result[item.Name] = item
		}
		if list.Continue == "" {
			break
		}
		if _, repeated := seenTokens[list.Continue]; repeated {
			return nil, errors.New("audit anchor: Kubernetes repeated a continuity-list continuation token")
		}
		seenTokens[list.Continue] = struct{}{}
		continueToken = list.Continue
	}
	return result, nil
}

func continuityIntentConfigMap(namespace string, intent model.AuditWitnessIntent) (*corev1.ConfigMap, error) {
	if err := validateContinuityIntent(intent); err != nil {
		return nil, err
	}
	record := continuityIntentRecord{
		Sequence: intent.Sequence, AuditID: intent.AuditID, AuditSHA256: hex.EncodeToString(intent.AuditSHA256),
		PreviousUUID: intent.PreviousUUID,
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		return nil, fmt.Errorf("audit anchor: encode witness intent: %w", err)
	}
	digest := sha256.Sum256(encoded)
	digestText := hex.EncodeToString(digest[:])
	immutable := true
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      continuityIntentNamePrefix + fmt.Sprintf("%04d-", intent.Sequence) + digestText[:24],
			Namespace: namespace,
			Labels: map[string]string{
				continuityLabelKey: continuityIntentLabelValue, continuityManagedByLabelKey: continuityManagedByLabelValue,
			},
		},
		Immutable: &immutable,
		Data:      map[string]string{"record.json": string(encoded), "sha256": digestText},
	}, nil
}

func validateContinuityIntent(intent model.AuditWitnessIntent) error {
	if intent.Sequence <= 0 || intent.AuditID < 0 || len(intent.AuditSHA256) != sha256.Size ||
		len(intent.PreviousUUID) > maximumWitnessUUIDBytes || strings.ContainsAny(intent.PreviousUUID, "\x00\r\n") {
		return fmt.Errorf("audit anchor: immutable witness intent %d has invalid fields", intent.Sequence)
	}
	if intent.Sequence == 1 && intent.PreviousUUID != "" {
		return errors.New("audit anchor: first immutable witness intent has a predecessor")
	}
	if intent.Sequence > 1 && intent.PreviousUUID == "" {
		return fmt.Errorf("audit anchor: immutable witness intent %d has no predecessor", intent.Sequence)
	}
	return nil
}

func verifyContinuitySet(existing map[string]*corev1.ConfigMap, expected map[string]*corev1.ConfigMap) error {
	for name, actual := range existing {
		want, ok := expected[name]
		if !ok {
			return fmt.Errorf("audit anchor: external history omitted immutable continuity record %s", name)
		}
		if !matchingContinuityConfigMap(actual, want) {
			return fmt.Errorf("audit anchor: immutable continuity record %s differs from verified Rekor history", name)
		}
	}
	return nil
}

func continuityConfigMap(namespace string, sequence int, witness model.AuditWitness) (*corev1.ConfigMap, error) {
	record := continuityRecord{
		Sequence: sequence, UUID: witness.UUID, LogIndex: witness.LogIndex,
		IntegratedAt: witness.IntegratedAt.UTC().Format(time.RFC3339Nano), AuditID: witness.AuditID,
		AuditSHA256: hex.EncodeToString(witness.AuditSHA256), PreviousUUID: witness.PreviousUUID,
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		return nil, fmt.Errorf("audit anchor: encode continuity record: %w", err)
	}
	digest := sha256.Sum256(encoded)
	digestText := hex.EncodeToString(digest[:])
	immutable := true
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      continuityNamePrefix + fmt.Sprintf("%04d-", sequence) + digestText[:24],
			Namespace: namespace,
			Labels: map[string]string{
				continuityLabelKey: continuityLabelValue, continuityManagedByLabelKey: continuityManagedByLabelValue,
			},
		},
		Immutable: &immutable,
		Data:      map[string]string{"record.json": string(encoded), "sha256": digestText},
	}, nil
}

func matchingContinuityConfigMap(actual, expected *corev1.ConfigMap) bool {
	if actual == nil || actual.Name != expected.Name || actual.Namespace != expected.Namespace || actual.Immutable == nil || !*actual.Immutable ||
		actual.DeletionTimestamp != nil || len(actual.OwnerReferences) != 0 || len(actual.Finalizers) != 0 || len(actual.BinaryData) != 0 ||
		actual.Labels[continuityLabelKey] != expected.Labels[continuityLabelKey] ||
		actual.Labels[continuityManagedByLabelKey] != expected.Labels[continuityManagedByLabelKey] {
		return false
	}
	return maps.Equal(actual.Data, expected.Data)
}

func validateContinuityHistory(history []model.AuditWitness) error {
	seen := make(map[string]struct{}, len(history))
	for index, witness := range history {
		if strings.TrimSpace(witness.UUID) != witness.UUID || witness.UUID == "" || len(witness.UUID) > maximumWitnessUUIDBytes ||
			strings.ContainsAny(witness.UUID, "\x00\r\n") || witness.LogIndex < 0 || witness.IntegratedAt.IsZero() || witness.IntegratedAt.UnixNano() <= 0 ||
			witness.AuditID < 0 || len(witness.AuditSHA256) != sha256.Size || len(witness.PreviousUUID) > maximumWitnessUUIDBytes ||
			strings.ContainsAny(witness.PreviousUUID, "\x00\r\n") {
			return fmt.Errorf("audit anchor: external witness %s has invalid continuity fields", strconv.Itoa(index+1))
		}
		if _, duplicate := seen[witness.UUID]; duplicate {
			return fmt.Errorf("audit anchor: duplicate external witness %s", witness.UUID)
		}
		seen[witness.UUID] = struct{}{}
		if index == 0 {
			if witness.PreviousUUID != "" {
				return errors.New("audit anchor: external witness history does not begin at its signed root")
			}
			continue
		}
		previous := history[index-1]
		if witness.PreviousUUID != previous.UUID {
			return fmt.Errorf("audit anchor: external witness %s does not link to predecessor %s", witness.UUID, previous.UUID)
		}
		if witness.AuditID <= previous.AuditID {
			return fmt.Errorf("audit anchor: external witness %s does not advance the audit sequence", witness.UUID)
		}
	}
	return nil
}

var _ Continuity = (*KubernetesContinuity)(nil)
