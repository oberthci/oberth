package service

import (
	"context"
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/oberthci/oberth/internal/store"
)

func testAccessReconciler(t *testing.T, cm *corev1.ConfigMap) (*AccessReconciler, *store.Store) {
	t.Helper()
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	database := testAccessStore(t, &now)
	var client *fake.Clientset
	if cm != nil {
		client = fake.NewClientset(cm)
	} else {
		client = fake.NewClientset()
	}
	logger := log.New(log.Writer(), "test: ", 0)
	reconciler := NewAccessReconciler(client, "oberth", database, logger)
	return reconciler, database
}

func testAccessStore(t *testing.T, now *time.Time) *store.Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "oberth.db")
	database, err := store.Open(context.Background(), path, store.Options{
		Now: func() time.Time { return *now },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := database.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	return database
}

func TestReconcileConfigMapWithGrants(t *testing.T) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:            secretAccessConfigMapName,
			Namespace:       "oberth",
			ResourceVersion: "42",
		},
		Data: map[string]string{
			secretAccessConfigMapKey: `- {repo: terraform, step: "*", secret: terraform/credentials}
- {repo: oberth, step: "*", secret: oberth/data/release/r2-upload-token}
`,
		},
	}
	reconciler, database := testAccessReconciler(t, cm)
	ctx := context.Background()

	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}

	grants, err := database.SecretAccessList(ctx, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(grants) != 2 {
		t.Fatalf("expected 2 grants, got %d", len(grants))
	}
	if grants[0].Repo != "oberth" || grants[0].Step != "*" || grants[0].Secret != "oberth/data/release/r2-upload-token" {
		t.Fatalf("unexpected grant[0]: %+v", grants[0])
	}
	if grants[1].Repo != "terraform" || grants[1].Step != "*" || grants[1].Secret != "terraform/credentials" {
		t.Fatalf("unexpected grant[1]: %+v", grants[1])
	}
	if grants[0].ApprovedBy != "configmap@rv=42" {
		t.Fatalf("approved_by = %q, want configmap@rv=42", grants[0].ApprovedBy)
	}
}

func TestReconcileConfigMapRemoved(t *testing.T) {
	reconciler, database := testAccessReconciler(t, nil)
	ctx := context.Background()

	// Seed a grant directly in sqlite.
	if _, err := database.Grant(ctx, "terraform", "*", "terraform/credentials", "admin@localhost"); err != nil {
		t.Fatal(err)
	}

	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}

	grants, err := database.SecretAccessList(ctx, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(grants) != 0 {
		t.Fatalf("expected 0 grants after ConfigMap removal, got %d", len(grants))
	}
}

func TestReconcileUnparseableConfigMap(t *testing.T) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:            secretAccessConfigMapName,
			Namespace:       "oberth",
			ResourceVersion: "99",
		},
		Data: map[string]string{
			secretAccessConfigMapKey: `this is not valid yaml: [[[`,
		},
	}
	reconciler, database := testAccessReconciler(t, cm)
	ctx := context.Background()

	// Seed a grant directly.
	if _, err := database.Grant(ctx, "terraform", "*", "terraform/credentials", "admin@localhost"); err != nil {
		t.Fatal(err)
	}

	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}

	// Fail-closed: all grants revoked.
	grants, err := database.SecretAccessList(ctx, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(grants) != 0 {
		t.Fatalf("expected 0 grants after unparseable ConfigMap, got %d", len(grants))
	}
}

func TestReconcileDiff(t *testing.T) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:            secretAccessConfigMapName,
			Namespace:       "oberth",
			ResourceVersion: "10",
		},
		Data: map[string]string{
			secretAccessConfigMapKey: `- {repo: terraform, step: "*", secret: terraform/credentials}
- {repo: oberth, step: "*", secret: cosign-secret}
`,
		},
	}
	reconciler, database := testAccessReconciler(t, cm)
	ctx := context.Background()

	// Seed grants: one matching, one that should be revoked.
	if _, err := database.Grant(ctx, "terraform", "*", "terraform/credentials", "admin@localhost"); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Grant(ctx, "terraform", "*", "terraform/state", "admin@localhost"); err != nil {
		t.Fatal(err)
	}

	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}

	grants, err := database.SecretAccessList(ctx, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(grants) != 2 {
		t.Fatalf("expected 2 active grants after diff, got %d", len(grants))
	}

	// Verify the kept grant and the new grant.
	grantMap := make(map[string]string)
	for _, g := range grants {
		grantMap[g.Repo+"/"+g.Step+"/"+g.Secret] = g.ApprovedBy
	}
	if _, ok := grantMap["terraform/*/terraform/credentials"]; !ok {
		t.Fatal("missing kept grant terraform/*/terraform/credentials")
	}
	if _, ok := grantMap["oberth/*/cosign-secret"]; !ok {
		t.Fatal("missing new grant oberth/*/cosign-secret")
	}

	// Verify the revoked grant.
	all, err := database.SecretAccessList(ctx, "terraform", true)
	if err != nil {
		t.Fatal(err)
	}
	revoked := 0
	for _, g := range all {
		if g.RevokedAt != nil {
			revoked++
		}
	}
	if revoked != 1 {
		t.Fatalf("expected 1 revoked grant, got %d", revoked)
	}
}

func TestReconcileIdempotent(t *testing.T) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:            secretAccessConfigMapName,
			Namespace:       "oberth",
			ResourceVersion: "5",
		},
		Data: map[string]string{
			secretAccessConfigMapKey: `- {repo: terraform, step: "*", secret: terraform/credentials}`,
		},
	}
	reconciler, database := testAccessReconciler(t, cm)
	ctx := context.Background()

	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}

	grants, err := database.SecretAccessList(ctx, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(grants) != 1 {
		t.Fatalf("expected 1 grant after double reconcile, got %d", len(grants))
	}
}

func TestUpdateConfigMapAllow(t *testing.T) {
	reconciler, database := testAccessReconciler(t, nil)
	ctx := context.Background()

	if err := reconciler.UpdateConfigMap(ctx, "", AddGrant("terraform", "*", "terraform/credentials")); err != nil {
		t.Fatal(err)
	}

	// Verify ConfigMap was created.
	cm, err := reconciler.client.CoreV1().ConfigMaps("oberth").Get(ctx, secretAccessConfigMapName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if cm.Data[secretAccessConfigMapKey] == "" {
		t.Fatal("ConfigMap grants data is empty")
	}

	// Verify sqlite was synced.
	grants, err := database.SecretAccessList(ctx, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(grants) != 1 {
		t.Fatalf("expected 1 grant after allow, got %d", len(grants))
	}
	if grants[0].Repo != "terraform" || grants[0].Step != "*" || grants[0].Secret != "terraform/credentials" {
		t.Fatalf("unexpected grant: %+v", grants[0])
	}
}

func TestUpdateConfigMapRevoke(t *testing.T) {
	// Start with a ConfigMap containing two grants.
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:            secretAccessConfigMapName,
			Namespace:       "oberth",
			ResourceVersion: "1",
		},
		Data: map[string]string{
			secretAccessConfigMapKey: `- {repo: terraform, step: "*", secret: terraform/credentials}
- {repo: oberth, step: "*", secret: cosign-secret}
`,
		},
	}
	reconciler, database := testAccessReconciler(t, cm)
	ctx := context.Background()

	// Initial reconcile to populate sqlite.
	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}

	// Revoke one grant via ConfigMap update.
	if err := reconciler.UpdateConfigMap(ctx, "", RemoveGrant("terraform", "*", "terraform/credentials")); err != nil {
		t.Fatal(err)
	}

	grants, err := database.SecretAccessList(ctx, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(grants) != 1 {
		t.Fatalf("expected 1 active grant after revoke, got %d", len(grants))
	}
	if grants[0].Repo != "oberth" || grants[0].Secret != "cosign-secret" {
		t.Fatalf("unexpected remaining grant: %+v", grants[0])
	}
}

func TestParseGrantsValidation(t *testing.T) {
	for _, test := range []struct {
		name string
		data string
		ok   bool
	}{
		{name: "empty", data: "", ok: true},
		{name: "valid wildcard step", data: `- {repo: a, step: "*", secret: c}`, ok: true},
		{name: "non-wildcard step rejected", data: `- {repo: a, step: build, secret: c}`, ok: false},
		{name: "wildcard secret", data: `- {repo: a, step: "*", secret: "*"}`, ok: false},
		{name: "wildcard repo", data: `- {repo: "*", step: "*", secret: c}`, ok: false},
		{name: "glob question in repo", data: `- {repo: "a?b", step: "*", secret: c}`, ok: false},
		{name: "glob bracket in repo", data: `- {repo: "a[b]", step: "*", secret: c}`, ok: false},
		{name: "hcl injection in repo", data: `- {repo: "x\" { capabilities = [\"sudo\"] }", step: "*", secret: c}`, ok: false},
		{name: "backslash in repo", data: `- {repo: "repo\\evil", step: "*", secret: c}`, ok: false},
		{name: "newline in repo", data: "- {repo: \"repo\\nevil\", step: \"*\", secret: c}", ok: false},
		{name: "braces in repo", data: `- {repo: "repo{evil}", step: "*", secret: c}`, ok: false},
		{name: "qualified repo valid", data: `- {repo: "codeberg/oberthci/oberth", step: "*", secret: c}`, ok: true},
		{name: "glob star in secret", data: `- {repo: a, step: "*", secret: "c*d"}`, ok: false},
		{name: "glob bracket in secret", data: `- {repo: a, step: "*", secret: "c[d]"}`, ok: false},
		{name: "path-like secret", data: `- {repo: a, step: "*", secret: "terraform/credentials"}`, ok: true},
		{name: "missing repo", data: `- {step: "*", secret: c}`, ok: false},
		{name: "missing step", data: `- {repo: a, secret: c}`, ok: false},
		{name: "missing secret", data: `- {repo: a, step: "*"}`, ok: false},
		{name: "invalid yaml", data: `[[[`, ok: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			entries, err := ParseGrants(test.data)
			if test.ok && err != nil {
				t.Fatalf("expected success, got %v", err)
			}
			if !test.ok && err == nil {
				t.Fatalf("expected error, got entries: %+v", entries)
			}
		})
	}
}

// TestParseGrantsRejectsNonWildcardStep proves that step values other than "*"
// are rejected at parse time, closing the gap where the grant model promises
// per-step scoping but the runtime cannot enforce it. Issue #27, finding 1.
func TestParseGrantsRejectsNonWildcardStep(t *testing.T) {
	for _, step := range []string{"build", "release", "test", "apply", "setup"} {
		data := fmt.Sprintf(`- {repo: terraform, step: %q, secret: terraform/credentials}`, step)
		if _, err := ParseGrants(data); err == nil {
			t.Fatalf("step %q was accepted; expected rejection", step)
		}
	}
	// The wildcard step must still work.
	if _, err := ParseGrants(`- {repo: terraform, step: "*", secret: terraform/credentials}`); err != nil {
		t.Fatalf("wildcard step was rejected: %v", err)
	}
}

// TestMalformedAllowPreservesExistingGrants proves that a malformed allow
// request (glob characters in repo/secret, or non-wildcard step) is rejected
// before the ConfigMap is modified, so existing valid grants are not revoked by
// a reconcile-to-zero triggered by the malformed entry. Issue #27, finding 12.
func TestMalformedAllowPreservesExistingGrants(t *testing.T) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:            secretAccessConfigMapName,
			Namespace:       "oberth",
			ResourceVersion: "50",
		},
		Data: map[string]string{
			secretAccessConfigMapKey: `- {repo: terraform, step: "*", secret: terraform/credentials}`,
		},
	}
	reconciler, database := testAccessReconciler(t, cm)
	ctx := context.Background()

	// Initial reconcile populates sqlite with the valid grant.
	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	grants, err := database.SecretAccessList(ctx, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(grants) != 1 {
		t.Fatalf("expected 1 grant before malformed allow, got %d", len(grants))
	}

	// Attempt to add a grant with glob characters in repo — must be rejected.
	if err := reconciler.UpdateConfigMap(ctx, "admin", AddGrant("terraform*", "*", "terraform/credentials")); err == nil {
		t.Fatal("expected a glob-in-repo grant to be rejected")
	}

	// Attempt to add a grant with a non-wildcard step — must be rejected.
	if err := reconciler.UpdateConfigMap(ctx, "admin", AddGrant("terraform", "build", "terraform/credentials")); err == nil {
		t.Fatal("expected a non-wildcard-step grant to be rejected")
	}

	// The original grant must still be active.
	grants, err = database.SecretAccessList(ctx, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(grants) != 1 {
		t.Fatalf("expected 1 grant after malformed allow attempt, got %d", len(grants))
	}
	if grants[0].Repo != "terraform" || grants[0].Secret != "terraform/credentials" {
		t.Fatalf("unexpected surviving grant: %+v", grants[0])
	}
}

func TestRemoveGrantNotFound(t *testing.T) {
	existing := []SecretAccessGrantEntry{
		{Repo: "a", Step: "b", Secret: "c"},
	}
	_, err := RemoveGrant("x", "y", "z")(existing)
	if err == nil {
		t.Fatal("expected error for missing grant")
	}
}

func TestUpdateConfigMapAttribution(t *testing.T) {
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	database := testAccessStore(t, &now)
	client := fake.NewClientset()
	logger := log.New(log.Writer(), "test: ", 0)
	reconciler := NewAccessReconciler(client, "oberth", database, logger)
	ctx := context.Background()

	// Allow with explicit actor — approved_by should carry the actor prefix.
	if err := reconciler.UpdateConfigMap(ctx, "agent@tuxbox", AddGrant("terraform", "*", "terraform/credentials")); err != nil {
		t.Fatal(err)
	}

	grants, err := database.SecretAccessList(ctx, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(grants) != 1 {
		t.Fatalf("expected 1 grant, got %d", len(grants))
	}
	if !strings.HasPrefix(grants[0].ApprovedBy, "agent@tuxbox+configmap@rv=") {
		t.Fatalf("approved_by = %q, want prefix agent@tuxbox+configmap@rv=", grants[0].ApprovedBy)
	}

	// Watcher-driven reconcile (no actor prefix) should use plain configmap@rv=N.
	cm, err := client.CoreV1().ConfigMaps("oberth").Get(ctx, secretAccessConfigMapName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cm.Data[secretAccessConfigMapKey] += "- {repo: oberth, step: \"*\", secret: cosign-secret}\n"
	if _, err := client.CoreV1().ConfigMaps("oberth").Update(ctx, cm, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}

	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}

	grants, err = database.SecretAccessList(ctx, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(grants) != 2 {
		t.Fatalf("expected 2 grants, got %d", len(grants))
	}
	for _, g := range grants {
		if g.Repo == "oberth" && g.Secret == "cosign-secret" {
			if strings.Contains(g.ApprovedBy, "agent@tuxbox") {
				t.Fatalf("watcher-path grant should not have actor prefix: approved_by = %q", g.ApprovedBy)
			}
			if !strings.HasPrefix(g.ApprovedBy, "configmap@rv=") {
				t.Fatalf("watcher-path approved_by = %q, want prefix configmap@rv=", g.ApprovedBy)
			}
		}
	}
}

func TestResyncSurvivesBrokenWatch(t *testing.T) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:            secretAccessConfigMapName,
			Namespace:       "oberth",
			ResourceVersion: "7",
		},
		Data: map[string]string{
			secretAccessConfigMapKey: `- {repo: terraform, step: "*", secret: terraform/credentials}`,
		},
	}
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	database := testAccessStore(t, &now)
	client := fake.NewClientset(cm)
	// Make Watch always fail while Get still works.
	client.PrependWatchReactor("configmaps", func(_ k8stesting.Action) (bool, watch.Interface, error) {
		return true, nil, fmt.Errorf("injected watch failure")
	})
	logger := log.New(log.Writer(), "test: ", 0)
	reconciler := NewAccessReconciler(client, "oberth", database, logger)
	reconciler.resyncInterval = 50 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	go func() { _ = reconciler.Watch(ctx) }()

	// Poll until sqlite converges despite broken watch.
	deadline := time.After(3 * time.Second)
	for {
		grants, err := database.SecretAccessList(context.Background(), "", false)
		if err != nil {
			t.Fatal(err)
		}
		if len(grants) == 1 && grants[0].Repo == "terraform" {
			return // converged
		}
		select {
		case <-deadline:
			t.Fatal("resync did not converge within 3 seconds despite broken watch")
		case <-time.After(25 * time.Millisecond):
		}
	}
}

func TestAddGrantIdempotent(t *testing.T) {
	existing := []SecretAccessGrantEntry{
		{Repo: "a", Step: "b", Secret: "c"},
	}
	result, err := AddGrant("a", "b", "c")(existing)
	if err != nil {
		t.Fatal(err)
	}
	if len(result) != 1 {
		t.Fatalf("expected 1 entry after idempotent add, got %d", len(result))
	}
}
