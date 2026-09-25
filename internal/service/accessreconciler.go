package service

import (
	"context"
	"errors"
	"fmt"
	"log"
	"regexp"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/yaml"

	"github.com/oberthci/oberth/internal/store"
)

const (
	secretAccessConfigMapName = "oberth-secret-access"
	secretAccessConfigMapKey  = "grants"
	watchResyncInterval       = 5 * time.Minute
	watchRetryDelay           = 5 * time.Second
)

// SecretAccessGrantEntry is the YAML representation of one grant in the
// ConfigMap's grants list.
type SecretAccessGrantEntry struct {
	Repo   string `json:"repo"   yaml:"repo"`
	Step   string `json:"step"   yaml:"step"`
	Secret string `json:"secret" yaml:"secret"`
}

// AccessReconciler synchronizes the oberth-secret-access ConfigMap with
// the sqlite secret_access table. It is the single writer for ConfigMap-driven
// grant management: the CLI and API update the ConfigMap, then call Reconcile.
type AccessReconciler struct {
	client         kubernetes.Interface
	namespace      string
	store          SecretAccessStore
	logger         *log.Logger
	mu             sync.Mutex
	resyncInterval time.Duration
	// reconcileSucceeded tracks whether at least one successful reconciliation
	// has completed. Credentialed admission must not proceed before the initial
	// reconcile succeeds, because a transient Get failure at startup would
	// leave stale grants active in sqlite — a failed reconciliation should
	// block credentialed submission rather than silently running with stale
	// state.
	reconcileSucceeded bool

	// OnReconcileSuccess is called after every successful reconciliation,
	// outside the reconciler's own lock. It is intended for downstream
	// consumers that need to recompute state derived from the grant table
	// (e.g. refreshing the per-repo identity store). Optional; nil is a
	// no-op. (Issue #465)
	OnReconcileSuccess func()
}

// NewAccessReconciler builds a reconciler that watches the named namespace
// for the oberth-secret-access ConfigMap.
func NewAccessReconciler(client kubernetes.Interface, namespace string, accessStore SecretAccessStore, logger *log.Logger) *AccessReconciler {
	return &AccessReconciler{
		client:    client,
		namespace: namespace,
		store:     accessStore,
		logger:    logger,
	}
}

// ReconcileHealthy reports whether at least one successful reconciliation has
// completed. Credentialed admission checks this to block jobs while the
// ConfigMap cannot be read — a transient initial Get failure must not leave
// stale sqlite grants active for admission to consume.
func (r *AccessReconciler) ReconcileHealthy() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.reconcileSucceeded
}

// Reconcile reads the ConfigMap and converges sqlite to match.
// Missing ConfigMap = zero grants (fail-closed).
// Unparseable ConfigMap = zero grants (fail-closed, log error).
func (r *AccessReconciler) Reconcile(ctx context.Context) error {
	r.mu.Lock()
	err := r.reconcileLocked(ctx, "")
	callback := r.OnReconcileSuccess
	r.mu.Unlock()
	if err == nil && callback != nil {
		callback()
	}
	return err
}

func (r *AccessReconciler) reconcileLocked(ctx context.Context, actorPrefix string) error {
	cm, err := r.client.CoreV1().ConfigMaps(r.namespace).Get(ctx, secretAccessConfigMapName, metav1.GetOptions{})
	if err != nil {
		if k8serrors.IsNotFound(err) {
			absentActor := "configmap@absent"
			if actorPrefix != "" {
				absentActor = actorPrefix + "+" + absentActor
			}
			r.logger.Printf("secret access ConfigMap %s/%s not found; reconciling to zero grants", r.namespace, secretAccessConfigMapName)
			if revokeErr := r.revokeAllLocked(ctx, absentActor); revokeErr != nil {
				return revokeErr
			}
			r.reconcileSucceeded = true
			return nil
		}
		return fmt.Errorf("get secret access ConfigMap: %w", err)
	}

	actor := fmt.Sprintf("configmap@rv=%s", cm.ResourceVersion)
	if actorPrefix != "" {
		actor = actorPrefix + "+" + actor
	}

	desired, err := ParseGrants(cm.Data[secretAccessConfigMapKey])
	if err != nil {
		r.logger.Printf("ERROR: unparseable grants in ConfigMap %s/%s: %v; reconciling to zero grants", r.namespace, secretAccessConfigMapName, err)
		if revokeErr := r.revokeAllLocked(ctx, actor); revokeErr != nil {
			return revokeErr
		}
		// An unparseable ConfigMap IS a successful reconciliation (to zero
		// grants). The health gate blocks on Get failure, not on content.
		r.reconcileSucceeded = true
		return nil
	}

	active, err := r.store.SecretAccessList(ctx, "", false)
	if err != nil {
		return fmt.Errorf("list active secret grants: %w", err)
	}

	// Canonicalize each entry's repository spelling BEFORE the converge
	// diff. The persisted grant key is the qualified "upstream/org/repo"
	// form (written by the v12 migration and the access allow/revoke
	// handlers); ConfigMap entries may carry any accepted spelling,
	// including the bare names every pre-G3 entry uses. Without this
	// resolution the exact-tuple diff would treat a bare entry and its
	// qualified sqlite row as different grants — revoking the qualified
	// row and re-creating it bare on every converge, silently undoing the
	// migration and reopening bare-key aliasing between same-name repos
	// (#245 BLOCKER B).
	//
	// Resolution semantics, fail-closed on ambiguity:
	//   - resolvable        -> qualified form keys the diff and the write
	//   - ErrAmbiguous      -> entry skipped loudly; an ambiguous bare
	//     spelling must never grant to either same-name repository
	//   - ErrNotFound/ErrInvalid -> entry kept verbatim; grants for
	//     unregistered repos are inert (admission resolves by repo ID and
	//     only ever consults qualified keys)
	desiredSet := make(map[string]SecretAccessGrantEntry, len(desired))
	for _, entry := range desired {
		canonical, ok, resolveErr := r.canonicalGrantRepo(ctx, entry.Repo)
		if resolveErr != nil {
			return fmt.Errorf("resolve grant repo %q: %w", entry.Repo, resolveErr)
		}
		if !ok {
			r.logger.Printf("ERROR: grant entry repo %q is ambiguous across upstreams; skipping entry (fail-closed) — respell it as upstream/org/repo in ConfigMap %s/%s",
				entry.Repo, r.namespace, secretAccessConfigMapName)
			continue
		}
		entry.Repo = canonical
		key := entry.Repo + "\x00" + entry.Step + "\x00" + entry.Secret
		desiredSet[key] = entry
	}

	activeSet := make(map[string]store.SecretAccessGrant, len(active))
	for _, grant := range active {
		key := grant.Repo + "\x00" + grant.Step + "\x00" + grant.Secret
		activeSet[key] = grant
	}

	// Add grants present in ConfigMap but missing from sqlite.
	for key, entry := range desiredSet {
		if _, exists := activeSet[key]; !exists {
			if _, err := r.store.Grant(ctx, entry.Repo, entry.Step, entry.Secret, actor); err != nil {
				return fmt.Errorf("grant %s/%s/%s: %w", entry.Repo, entry.Step, entry.Secret, err)
			}
		}
	}

	// Revoke grants present in sqlite but absent from ConfigMap.
	for key, grant := range activeSet {
		if _, exists := desiredSet[key]; !exists {
			if _, err := r.store.Revoke(ctx, grant.Repo, grant.Step, grant.Secret, actor); err != nil {
				return fmt.Errorf("revoke %s/%s/%s: %w", grant.Repo, grant.Step, grant.Secret, err)
			}
		}
	}

	r.reconcileSucceeded = true
	return nil
}

// canonicalGrantRepo resolves one ConfigMap grant entry's repository
// spelling to the qualified "upstream/org/repo" persisted form. The second
// return is false only for an ambiguous bare spelling (two same-name repos
// under different upstreams), which the caller must skip fail-closed. An
// unregistered or syntactically unresolvable spelling is returned verbatim:
// such grants are inert because admission looks grants up by repository ID
// through the qualified key only. Any other store error aborts the
// reconcile so it retries rather than converging on partial state.
func (r *AccessReconciler) canonicalGrantRepo(ctx context.Context, repo string) (string, bool, error) {
	repository, err := r.store.RepositoryByName(ctx, repo)
	if err != nil {
		if errors.Is(err, store.ErrAmbiguous) {
			return "", false, nil
		}
		if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrInvalid) {
			return repo, true, nil
		}
		return "", false, err
	}
	qualified, err := r.store.QualifiedRepoName(ctx, repository.ID)
	if err != nil {
		return "", false, err
	}
	return qualified, true, nil
}

func (r *AccessReconciler) revokeAllLocked(ctx context.Context, actor string) error {
	active, err := r.store.SecretAccessList(ctx, "", false)
	if err != nil {
		return fmt.Errorf("list active grants for revocation: %w", err)
	}
	for _, grant := range active {
		if _, err := r.store.Revoke(ctx, grant.Repo, grant.Step, grant.Secret, actor); err != nil {
			return fmt.Errorf("revoke %s/%s/%s: %w", grant.Repo, grant.Step, grant.Secret, err)
		}
	}
	return nil
}

// Watch runs the ConfigMap watch loop with periodic resync. It blocks until
// ctx is cancelled or a fatal error occurs. Transient watch failures are
// retried after a short delay.
func (r *AccessReconciler) Watch(ctx context.Context) error {
	interval := r.resyncInterval
	if interval <= 0 {
		interval = watchResyncInterval
	}
	resync := time.NewTicker(interval)
	defer resync.Stop()

	for {
		if err := r.watchOnce(ctx, resync.C); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			r.logger.Printf("secret access ConfigMap watch error: %v; retrying", err)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-resync.C:
				if err := r.Reconcile(ctx); err != nil {
					r.logger.Printf("resync reconcile error during watch retry: %v", err)
				}
			case <-time.After(watchRetryDelay):
			}
			continue
		}
	}
}

func (r *AccessReconciler) watchOnce(ctx context.Context, resync <-chan time.Time) error {
	// Reconcile before establishing the watch so a revocation that occurred
	// during the gap between the old watch closing and this new one starting
	// is applied before any credentialed submission is allowed. Without this
	// a revocation in the gap would stay active until the next periodic resync.
	if err := r.Reconcile(ctx); err != nil {
		r.logger.Printf("reconcile on watch reconnect error: %v", err)
	}

	watcher, err := r.client.CoreV1().ConfigMaps(r.namespace).Watch(ctx, metav1.ListOptions{
		FieldSelector: "metadata.name=" + secretAccessConfigMapName,
	})
	if err != nil {
		return fmt.Errorf("start ConfigMap watch: %w", err)
	}
	defer watcher.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-resync:
			if err := r.Reconcile(ctx); err != nil {
				r.logger.Printf("resync reconcile error: %v", err)
			}
		case event, ok := <-watcher.ResultChan():
			if !ok {
				return nil // watch closed, caller retries
			}
			switch event.Type {
			case watch.Added, watch.Modified, watch.Deleted:
				if err := r.Reconcile(ctx); err != nil {
					r.logger.Printf("reconcile on ConfigMap %s error: %v", event.Type, err)
				}
			case watch.Error:
				return fmt.Errorf("watch error event received")
			}
		}
	}
}

// UpdateConfigMap reads, modifies, and writes back the ConfigMap with a
// resourceVersion-checked update to prevent conflicts. The modify function
// receives the current grants (nil when the ConfigMap does not exist) and
// returns the desired grants. After the ConfigMap is updated, a reconcile
// is triggered to sync sqlite.
func (r *AccessReconciler) UpdateConfigMap(ctx context.Context, actor string, modify func([]SecretAccessGrantEntry) ([]SecretAccessGrantEntry, error)) (retErr error) {
	r.mu.Lock()
	defer func() {
		callback := r.OnReconcileSuccess
		r.mu.Unlock()
		if retErr == nil && callback != nil {
			callback()
		}
	}()

	cm, err := r.client.CoreV1().ConfigMaps(r.namespace).Get(ctx, secretAccessConfigMapName, metav1.GetOptions{})
	if err != nil {
		if !k8serrors.IsNotFound(err) {
			return fmt.Errorf("get ConfigMap for update: %w", err)
		}
		// ConfigMap does not exist yet; create it.
		entries, modifyErr := modify(nil)
		if modifyErr != nil {
			return modifyErr
		}
		for i, entry := range entries {
			if err := ValidateGrantEntry(i, entry); err != nil {
				return fmt.Errorf("validate new grants: %w", err)
			}
		}
		data, marshalErr := MarshalGrants(entries)
		if marshalErr != nil {
			return marshalErr
		}
		cm = &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      secretAccessConfigMapName,
				Namespace: r.namespace,
			},
			Data: map[string]string{secretAccessConfigMapKey: data},
		}
		if _, createErr := r.client.CoreV1().ConfigMaps(r.namespace).Create(ctx, cm, metav1.CreateOptions{}); createErr != nil {
			return fmt.Errorf("create ConfigMap: %w", createErr)
		}
		return r.reconcileLocked(ctx, actor)
	}

	existing, parseErr := ParseGrants(cm.Data[secretAccessConfigMapKey])
	if parseErr != nil {
		return fmt.Errorf("parse existing grants: %w", parseErr)
	}

	updated, err := modify(existing)
	if err != nil {
		return err
	}

	// Validate the complete modified list before writing, so a single
	// malformed entry cannot reach the ConfigMap and trigger a
	// reconcile-to-zero that revokes every active approval.
	for i, entry := range updated {
		if err := ValidateGrantEntry(i, entry); err != nil {
			return fmt.Errorf("validate modified grants: %w", err)
		}
	}

	data, err := MarshalGrants(updated)
	if err != nil {
		return err
	}

	if cm.Data == nil {
		cm.Data = make(map[string]string)
	}
	cm.Data[secretAccessConfigMapKey] = data
	if _, updateErr := r.client.CoreV1().ConfigMaps(r.namespace).Update(ctx, cm, metav1.UpdateOptions{}); updateErr != nil {
		return fmt.Errorf("update ConfigMap: %w", updateErr)
	}
	return r.reconcileLocked(ctx, actor)
}

// grantEntryRepoPattern matches valid characters for the repo field of a grant
// entry. Repo names are qualified (upstream/org/repo) and are eventually
// interpolated into Vault policy HCL; quotes, backslashes, newlines, braces,
// and control characters would escape the HCL path string boundary and inject
// arbitrary policy rules (issue #416, same class as #411 and #414).
var grantEntryRepoPattern = regexp.MustCompile(`^[A-Za-z0-9._/-]+$`)

// ValidateGrantEntry checks one grant entry for validity. It is shared by
// ParseGrants (ConfigMap reconciliation) and the API (before any ConfigMap
// mutation), so a malformed entry is rejected before it can trigger a
// reconcile-to-zero that revokes every active approval.
func ValidateGrantEntry(index int, entry SecretAccessGrantEntry) error {
	if strings.TrimSpace(entry.Repo) == "" || strings.TrimSpace(entry.Step) == "" || strings.TrimSpace(entry.Secret) == "" {
		return fmt.Errorf("entry %d: repo, step, and secret are required", index)
	}
	if !grantEntryRepoPattern.MatchString(entry.Repo) {
		return fmt.Errorf("entry %d: repo %q contains characters outside [A-Za-z0-9._/-]; refusing to persist (HCL injection risk)", index, entry.Repo)
	}
	if strings.ContainsAny(entry.Secret, "*?[]") {
		return fmt.Errorf("entry %d: wildcard and glob characters are not allowed in secret", index)
	}
	// Until the runtime can enforce grants per Argo template, the step
	// dimension is not enforceable at the Workflow level — a template
	// granted to step "release" would receive the credential regardless of
	// which DAG task invokes it. Accept only the explicit wildcard "*" so
	// existing non-wildcard entries fail closed rather than granting wider
	// access than the entry's author intended.
	if entry.Step != "*" {
		return fmt.Errorf("entry %d: step must be \"*\" (per-step grants are not yet enforceable at runtime; use \"*\" to grant to all steps)", index)
	}
	return nil
}

// ParseGrants parses the YAML grants list from ConfigMap data. An empty
// string returns nil with no error (zero grants).
func ParseGrants(data string) ([]SecretAccessGrantEntry, error) {
	if strings.TrimSpace(data) == "" {
		return nil, nil
	}
	var entries []SecretAccessGrantEntry
	if err := yaml.Unmarshal([]byte(data), &entries); err != nil {
		return nil, err
	}
	for i, entry := range entries {
		if err := ValidateGrantEntry(i, entry); err != nil {
			return nil, err
		}
	}
	return entries, nil
}

// MarshalGrants serializes grants to YAML for ConfigMap storage. The "secret"
// field is a path identifier (e.g. "terraform/credentials"), not a credential.
func MarshalGrants(entries []SecretAccessGrantEntry) (string, error) {
	if len(entries) == 0 {
		return "", nil
	}
	// #nosec G117 -- "Secret" is a path identifier, not a credential value.
	data, err := yaml.Marshal(entries)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// AddGrant returns a modify function that adds one grant entry, deduplicating
// against existing entries.
func AddGrant(repo, step, secret string) func([]SecretAccessGrantEntry) ([]SecretAccessGrantEntry, error) {
	return func(existing []SecretAccessGrantEntry) ([]SecretAccessGrantEntry, error) {
		for _, entry := range existing {
			if entry.Repo == repo && entry.Step == step && entry.Secret == secret {
				return existing, nil // already present
			}
		}
		return append(existing, SecretAccessGrantEntry{Repo: repo, Step: step, Secret: secret}), nil
	}
}

// RemoveGrant returns a modify function that removes one grant entry.
// Returns an error if the entry is not found.
func RemoveGrant(repo, step, secret string) func([]SecretAccessGrantEntry) ([]SecretAccessGrantEntry, error) {
	return func(existing []SecretAccessGrantEntry) ([]SecretAccessGrantEntry, error) {
		result := make([]SecretAccessGrantEntry, 0, len(existing))
		found := false
		for _, entry := range existing {
			if entry.Repo == repo && entry.Step == step && entry.Secret == secret {
				found = true
				continue
			}
			result = append(result, entry)
		}
		if !found {
			return nil, fmt.Errorf("no grant for %s/%s/%s in ConfigMap", repo, step, secret)
		}
		return result, nil
	}
}
