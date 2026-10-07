package installer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// --- ConfigMap fallback path (run == nil) ---

func TestProducePerRepoIdentitiesFromQualifiedGrants(t *testing.T) {
	t.Parallel()
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: secretAccessConfigMapName, Namespace: "oberth"},
		Data: map[string]string{
			secretAccessConfigMapKey: `
- repo: codeberg/oberthci/oberth
  step: "*"
  secret: oberth/data/release/cosign-secret
- repo: codeberg/oberthci/oberth
  step: "*"
  secret: oberth/data/release/r2-token
- repo: github/skipops/terraform
  step: "*"
  secret: oberth/upstream/skipops/terraform/plan/gcp-sa
`,
		},
	}
	kube := fake.NewSimpleClientset(cm)

	result, warnings, err := ProducePerRepoIdentities(context.Background(), kube, nil, "", "oberth")
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", warnings)
	}

	if len(result) != 2 {
		t.Fatalf("expected 2 per-repo identities, got %d: %+v", len(result), result)
	}

	// Sort for deterministic assertion.
	sort.Slice(result, func(i, j int) bool {
		return result[i].Repo < result[j].Repo
	})

	// First: oberth (codeberg/oberthci)
	if result[0].Upstream != "codeberg" || result[0].Org != "oberthci" || result[0].Repo != "oberth" {
		t.Fatalf("unexpected identity[0]: %+v", result[0])
	}
	if len(result[0].Grants) != 2 {
		t.Fatalf("expected 2 grants for oberth, got %d", len(result[0].Grants))
	}

	// Second: terraform (github/skipops)
	if result[1].Upstream != "github" || result[1].Org != "skipops" || result[1].Repo != "terraform" {
		t.Fatalf("unexpected identity[1]: %+v", result[1])
	}
}

func TestProducePerRepoIdentitiesSkipsBareRepoNames(t *testing.T) {
	t.Parallel()
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: secretAccessConfigMapName, Namespace: "oberth"},
		Data: map[string]string{
			secretAccessConfigMapKey: `
- repo: oberth
  step: "*"
  secret: oberth/data/release/cosign-secret
- repo: codeberg/oberthci/oberth
  step: "*"
  secret: oberth/data/release/r2-token
`,
		},
	}
	kube := fake.NewSimpleClientset(cm)

	result, _, err := ProducePerRepoIdentities(context.Background(), kube, nil, "", "oberth")
	if err != nil {
		t.Fatal(err)
	}

	// Only the qualified entry should produce an identity.
	if len(result) != 1 {
		t.Fatalf("expected 1 identity (bare name skipped), got %d: %+v", len(result), result)
	}
	if result[0].Repo != "oberth" || result[0].Upstream != "codeberg" {
		t.Fatalf("unexpected identity: %+v", result[0])
	}
}

func TestProducePerRepoIdentitiesReturnsNilWhenConfigMapAbsent(t *testing.T) {
	t.Parallel()
	kube := fake.NewSimpleClientset()

	result, warnings, err := ProducePerRepoIdentities(context.Background(), kube, nil, "", "oberth")
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", warnings)
	}
	if result != nil {
		t.Fatalf("expected nil, got %+v", result)
	}
}

func TestProducePerRepoIdentitiesReturnsNilOnEmptyData(t *testing.T) {
	t.Parallel()
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: secretAccessConfigMapName, Namespace: "oberth"},
		Data:       map[string]string{secretAccessConfigMapKey: ""},
	}
	kube := fake.NewSimpleClientset(cm)

	result, _, err := ProducePerRepoIdentities(context.Background(), kube, nil, "", "oberth")
	if err != nil {
		t.Fatal(err)
	}
	if result != nil {
		t.Fatalf("expected nil, got %+v", result)
	}
}

// --- Exec path (access list parsing — tabwriter) ---

func TestProduceFromAccessListParsesQualifiedOutput(t *testing.T) {
	t.Parallel()
	// Simulate the tabwriter-formatted output of `oberth access list`.
	accessListOutput := strings.Join([]string{
		"REPO                                        STEP   SECRET                                             APPROVED BY                            APPROVED AT        STATUS",
		"codeberg/acme/acme-beacon       *      oberth/data/release/cosign-secret                  admin@localhost+configmap@rv=225951    2026-08-21 16:40   active",
		"codeberg/acme/acme-beacon       *      oberth/data/release/gar-sa-key                     admin@localhost+configmap@rv=225949    2026-08-21 16:40   active",
		"codeberg/acme/terraform               *      oberth/upstream/acme/terraform/credentials   configmap@rv=699072                    2026-08-23 22:23   active",
		"github/oberthci/oberth                      *      oberth/data/release/cosign-secret                  configmap@rv=18991                     2026-08-18 08:54   active",
		"github/oberthci/oberth                      *      oberth/data/release/r2-upload-token                admin@localhost+configmap@rv=18990     2026-08-18 08:54   active",
	}, "\n")

	// Mock the CommandRunner. First call (--json) fails to simulate an older
	// deployed binary; second call (tabwriter) succeeds.
	callCount := 0
	mockRun := func(_ context.Context, _ []byte, _ string, args ...string) ([]byte, error) {
		callCount++
		for _, a := range args {
			if a == "--json" {
				return nil, fmt.Errorf("flag provided but not defined: -json")
			}
		}
		return []byte(accessListOutput), nil
	}

	result, _, err := ProducePerRepoIdentities(context.Background(), nil, mockRun, "", "oberth")
	if err != nil {
		t.Fatal(err)
	}

	if len(result) != 3 {
		t.Fatalf("expected 3 per-repo identities, got %d: %+v", len(result), result)
	}

	sort.Slice(result, func(i, j int) bool {
		key := func(id PerRepoIdentity) string { return id.Upstream + "/" + id.Org + "/" + id.Repo }
		return key(result[i]) < key(result[j])
	})

	// acme-beacon: 2 grants
	if result[0].Upstream != "codeberg" || result[0].Org != "acme" || result[0].Repo != "acme-beacon" {
		t.Fatalf("unexpected identity[0]: %+v", result[0])
	}
	if len(result[0].Grants) != 2 {
		t.Fatalf("expected 2 grants for acme-beacon, got %d: %v", len(result[0].Grants), result[0].Grants)
	}

	// terraform: 1 upstream grant
	if result[1].Upstream != "codeberg" || result[1].Org != "acme" || result[1].Repo != "terraform" {
		t.Fatalf("unexpected identity[1]: %+v", result[1])
	}
	if len(result[1].Grants) != 1 || result[1].Grants[0] != "oberth/upstream/acme/terraform/credentials" {
		t.Fatalf("unexpected grants for terraform: %v", result[1].Grants)
	}

	// oberth: 2 grants
	if result[2].Upstream != "github" || result[2].Org != "oberthci" || result[2].Repo != "oberth" {
		t.Fatalf("unexpected identity[2]: %+v", result[2])
	}
	if len(result[2].Grants) != 2 {
		t.Fatalf("expected 2 grants for oberth, got %d", len(result[2].Grants))
	}
}

func TestProduceFromAccessListFallsBackToConfigMapOnExecFailure(t *testing.T) {
	t.Parallel()
	// Exec fails — simulate a pod that is not running.
	failRun := func(_ context.Context, _ []byte, _ string, _ ...string) ([]byte, error) {
		return nil, context.DeadlineExceeded
	}

	// ConfigMap with a qualified entry should still be found via fallback.
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: secretAccessConfigMapName, Namespace: "oberth"},
		Data: map[string]string{
			secretAccessConfigMapKey: `
- repo: codeberg/oberthci/oberth
  step: "*"
  secret: oberth/data/release/cosign-secret
`,
		},
	}
	kube := fake.NewSimpleClientset(cm)

	result, warnings, err := ProducePerRepoIdentities(context.Background(), kube, failRun, "", "oberth")
	if err != nil {
		t.Fatal(err)
	}
	if len(result) != 1 {
		t.Fatalf("expected 1 identity from CM fallback, got %d: %+v", len(result), result)
	}
	if result[0].Repo != "oberth" {
		t.Fatalf("unexpected identity: %+v", result[0])
	}
	// The exec→CM fallback warning must be surfaced.
	if len(warnings) == 0 {
		t.Fatal("expected at least one warning about exec→CM fallback")
	}
	found := false
	for _, w := range warnings {
		if strings.Contains(w, "fell back to ConfigMap") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected 'fell back to ConfigMap' warning, got: %v", warnings)
	}
}

func TestParseAccessListOutputSkipsBareRows(t *testing.T) {
	t.Parallel()
	output := strings.Join([]string{
		"REPO          STEP   SECRET                              APPROVED BY   APPROVED AT        STATUS",
		"terraform     *      oberth/data/release/cosign-secret   admin         2026-08-18 08:54   active",
	}, "\n")

	_, err := ParseAccessListOutput([]byte(output))
	if err == nil || !strings.Contains(err.Error(), "format drift") {
		t.Fatalf("expected format-drift error for all-bare rows, got: %v", err)
	}
}

func TestParseAccessListOutputEmptyInput(t *testing.T) {
	t.Parallel()
	result, err := ParseAccessListOutput([]byte(""))
	if err != nil {
		t.Fatal(err)
	}
	if len(result) != 0 {
		t.Fatalf("expected 0, got %d", len(result))
	}
}

// --- JSON path tests ---

func TestParseAccessListJSONQualifiedEntries(t *testing.T) {
	t.Parallel()
	entries := []accessListJSONEntry{
		{Repo: "codeberg/oberthci/oberth", Step: "release", Secret: "oberth/data/release/cosign-secret"},
		{Repo: "codeberg/oberthci/oberth", Step: "release", Secret: "oberth/data/release/r2-upload-token"},
		{Repo: "github/skipops/terraform", Step: "*", Secret: "oberth/upstream/skipops/terraform/gcp-sa"},
	}
	data, _ := json.Marshal(entries)
	result, err := ParseAccessListJSON(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(result) != 2 {
		t.Fatalf("expected 2 identities, got %d: %+v", len(result), result)
	}
}

func TestParseAccessListJSONEmptyArray(t *testing.T) {
	t.Parallel()
	result, err := ParseAccessListJSON([]byte("[]"))
	if err != nil {
		t.Fatal(err)
	}
	if len(result) != 0 {
		t.Fatalf("expected 0 identities from empty array, got %d", len(result))
	}
}

func TestParseAccessListJSONAllBareReturnsError(t *testing.T) {
	t.Parallel()
	entries := []accessListJSONEntry{
		{Repo: "terraform", Step: "*", Secret: "oberth/data/release/cosign-secret"},
	}
	data, _ := json.Marshal(entries)
	_, err := ParseAccessListJSON(data)
	if err == nil || !strings.Contains(err.Error(), "format drift") {
		t.Fatalf("expected format-drift error for all-bare JSON entries, got: %v", err)
	}
}

func TestProduceFromAccessListPrefersJSON(t *testing.T) {
	t.Parallel()
	jsonEntries := []accessListJSONEntry{
		{Repo: "codeberg/oberthci/oberth", Step: "release", Secret: "oberth/data/release/cosign-secret"},
	}
	jsonData, _ := json.Marshal(jsonEntries)

	// Mock returns JSON when --json is requested, tabwriter otherwise.
	mockRun := func(_ context.Context, _ []byte, _ string, args ...string) ([]byte, error) {
		for _, a := range args {
			if a == "--json" {
				return jsonData, nil
			}
		}
		return nil, fmt.Errorf("should not reach tabwriter path")
	}

	result, warnings, err := ProducePerRepoIdentities(context.Background(), nil, mockRun, "", "oberth")
	if err != nil {
		t.Fatal(err)
	}
	if len(result) != 1 {
		t.Fatalf("expected 1 identity from JSON path, got %d: %+v", len(result), result)
	}
	if result[0].Repo != "oberth" || result[0].Upstream != "codeberg" {
		t.Fatalf("unexpected identity from JSON path: %+v", result[0])
	}
	// No degradation warnings expected when JSON path succeeds.
	for _, w := range warnings {
		if strings.Contains(w, "fell back") {
			t.Fatalf("unexpected fallback warning: %s", w)
		}
	}
}

// --- CM RBAC error surfacing (gap 4) ---

func TestProduceFromConfigMapSurfacesRBACError(t *testing.T) {
	t.Parallel()
	kube := fake.NewSimpleClientset()
	kube.PrependReactor("get", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(
			schema.GroupResource{Group: "", Resource: "configmaps"}, secretAccessConfigMapName,
			fmt.Errorf("RBAC: access denied"),
		)
	})

	_, _, err := ProducePerRepoIdentities(context.Background(), kube, nil, "", "oberth")
	if err == nil {
		t.Fatal("expected RBAC error to surface, got nil")
	}
	if !strings.Contains(err.Error(), "RBAC") && !strings.Contains(err.Error(), "Forbidden") {
		t.Fatalf("error should mention RBAC/Forbidden, got: %v", err)
	}
}

func TestProduceFromConfigMapIsNotFoundReturnsNilNil(t *testing.T) {
	t.Parallel()
	// A simple fake clientset with no ConfigMaps returns IsNotFound for Get.
	kube := fake.NewSimpleClientset()

	result, warnings, err := ProducePerRepoIdentities(context.Background(), kube, nil, "", "oberth")
	if err != nil {
		t.Fatalf("IsNotFound should not surface as error, got: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", warnings)
	}
	if result != nil {
		t.Fatalf("expected nil, got %+v", result)
	}
}

// --- Exec garbage: valid exit, garbage data → error → CM fallback → warning ---

func TestProduceExecGarbageTriggersConfigMapFallback(t *testing.T) {
	t.Parallel()
	// Both --json and tabwriter exec succeed but with garbage data. The JSON
	// parse fails, tabwriter yields data lines but no qualified identities
	// (triggers format-drift error), so the producer falls through to the
	// ConfigMap.
	mockRun := func(_ context.Context, _ []byte, _ string, args ...string) ([]byte, error) {
		for _, a := range args {
			if a == "--json" {
				return []byte("not valid json"), nil
			}
		}
		// Tabwriter: valid exit, data lines, but no qualified repos.
		return []byte("REPO   STEP   SECRET   APPROVED BY   APPROVED AT   STATUS\ngarbage   *   some/secret   admin   2026-08-18   active\n"), nil
	}

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: secretAccessConfigMapName, Namespace: "oberth"},
		Data: map[string]string{
			secretAccessConfigMapKey: `
- repo: codeberg/oberthci/oberth
  step: "*"
  secret: oberth/data/release/cosign-secret
`,
		},
	}
	kube := fake.NewSimpleClientset(cm)

	result, warnings, err := ProducePerRepoIdentities(context.Background(), kube, mockRun, "", "oberth")
	if err != nil {
		t.Fatalf("should have fallen back to CM, got error: %v", err)
	}
	if len(result) != 1 || result[0].Repo != "oberth" {
		t.Fatalf("expected 1 identity from CM fallback, got %d: %+v", len(result), result)
	}
	// Must have a warning about the exec→CM fallback.
	found := false
	for _, w := range warnings {
		if strings.Contains(w, "fell back to ConfigMap") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected 'fell back to ConfigMap' warning, got: %v", warnings)
	}
}

func TestProduceExecTabwriterGarbageTriggersConfigMapFallback(t *testing.T) {
	t.Parallel()
	// --json exec fails (old binary), tabwriter exec succeeds but returns
	// only bare (unqualified) repo names — triggers the format-drift error
	// in parseAccessListOutput, which causes the exec path to fail and the
	// producer to fall through to ConfigMap.
	mockRun := func(_ context.Context, _ []byte, _ string, args ...string) ([]byte, error) {
		for _, a := range args {
			if a == "--json" {
				return nil, fmt.Errorf("flag provided but not defined: -json")
			}
		}
		return []byte("REPO          STEP   SECRET                              APPROVED BY   APPROVED AT        STATUS\ngarbage       *      some/secret                         admin         2026-08-18 08:54   active\n"), nil
	}

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: secretAccessConfigMapName, Namespace: "oberth"},
		Data: map[string]string{
			secretAccessConfigMapKey: "- repo: codeberg/oberthci/oberth\n  step: \"*\"\n  secret: oberth/data/release/cosign-secret\n",
		},
	}
	kube := fake.NewSimpleClientset(cm)

	result, warnings, err := ProducePerRepoIdentities(context.Background(), kube, mockRun, "", "oberth")
	if err != nil {
		t.Fatalf("should have fallen back to CM, got error: %v", err)
	}
	if len(result) != 1 || result[0].Repo != "oberth" {
		t.Fatalf("expected 1 identity from CM fallback, got %d: %+v", len(result), result)
	}
	found := false
	for _, w := range warnings {
		if strings.Contains(w, "fell back to ConfigMap") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected fell-back-to-ConfigMap warning, got: %v", warnings)
	}
}

// --- Parse-to-zero with non-empty input = error ---

func TestParseAccessListOutputDataLinesNoQualifiedReturnsError(t *testing.T) {
	t.Parallel()
	// Multiple data lines, none qualified — should return format-drift error.
	output := strings.Join([]string{
		"REPO          STEP   SECRET                              APPROVED BY   APPROVED AT        STATUS",
		"terraform     *      oberth/data/release/cosign-secret   admin         2026-08-18 08:54   active",
		"oberth        *      oberth/data/release/r2-token        admin         2026-08-18 08:54   active",
	}, "\n")
	_, err := ParseAccessListOutput([]byte(output))
	if err == nil {
		t.Fatal("expected error for data lines with no qualified identities")
	}
	if !strings.Contains(err.Error(), "2 data lines") {
		t.Fatalf("error should mention 2 data lines, got: %v", err)
	}
}

func TestParseAccessListOutputHeaderOnlyIsEmpty(t *testing.T) {
	t.Parallel()
	output := "REPO   STEP   SECRET   APPROVED BY   APPROVED AT   STATUS\n"
	result, err := ParseAccessListOutput([]byte(output))
	if err != nil {
		t.Fatal(err)
	}
	if len(result) != 0 {
		t.Fatalf("expected 0, got %d", len(result))
	}
}

func TestParseAccessListJSONEntriesNoQualifiedReturnsError(t *testing.T) {
	t.Parallel()
	entries := []accessListJSONEntry{
		{Repo: "terraform", Step: "*", Secret: "s1"},
		{Repo: "oberth", Step: "release", Secret: "s2"},
	}
	data, _ := json.Marshal(entries)
	_, err := ParseAccessListJSON(data)
	if err == nil {
		t.Fatal("expected error for JSON entries with no qualified identities")
	}
	if !strings.Contains(err.Error(), "2 entries") {
		t.Fatalf("error should mention 2 entries, got: %v", err)
	}
}

// --- Integration: producer output through ConfigurePerRepoIdentities ---
// This proves that the producer's output format (including upstream-prefix
// grants) is accepted by the downstream consumer without error, closing
// the test gap identified in the review.

func TestProducerOutputAcceptedByConfigurePerRepoIdentities(t *testing.T) {
	t.Parallel()

	// Simulate access list output with both data-prefix and upstream-prefix grants.
	accessListOutput := strings.Join([]string{
		"REPO                                        STEP   SECRET                                             APPROVED BY       APPROVED AT        STATUS",
		"codeberg/acme/acme-beacon       *      oberth/data/release/cosign-secret                  admin             2026-08-21 16:40   active",
		"codeberg/acme/acme-beacon       *      oberth/data/release/gar-sa-key                     admin             2026-08-21 16:40   active",
		"codeberg/acme/terraform               *      oberth/upstream/acme/terraform/credentials   admin             2026-08-23 22:23   active",
	}, "\n")

	// Mock runner: --json fails (old binary), tabwriter succeeds.
	mockRun := func(_ context.Context, _ []byte, _ string, args ...string) ([]byte, error) {
		for _, a := range args {
			if a == "--json" {
				return nil, fmt.Errorf("flag provided but not defined: -json")
			}
		}
		return []byte(accessListOutput), nil
	}

	identities, _, err := ProducePerRepoIdentities(context.Background(), nil, mockRun, "", "oberth")
	if err != nil {
		t.Fatal(err)
	}

	if len(identities) != 2 {
		t.Fatalf("expected 2 identities, got %d: %+v", len(identities), identities)
	}

	// Script bao responses for both identities' policy + role operations,
	// including the CI-tier per-repo identities added by issue #433.
	beaconName := PerRepoName("codeberg", "acme", "acme-beacon")
	terraformName := PerRepoName("codeberg", "acme", "terraform")
	beaconCIName := PerRepoCIName("codeberg", "acme", "acme-beacon")
	terraformCIName := PerRepoCIName("codeberg", "acme", "terraform")

	responses := map[string]fakeBaoResponse{
		// beacon: release policy and role not yet present
		"policy read " + beaconName:                            {out: "No policy named: " + beaconName, err: errors.New("exit status 2")},
		"policy write " + beaconName + " -":                    {out: "Success!"},
		"read -format=json auth/kubernetes/role/" + beaconName: {out: "No value found", err: errors.New("exit status 2")},
		"write auth/kubernetes/role/" + beaconName + " -":      {out: "Success!"},
		// beacon: CI policy and role not yet present
		"policy read " + beaconCIName:                            {out: "No policy named: " + beaconCIName, err: errors.New("exit status 2")},
		"policy write " + beaconCIName + " -":                    {out: "Success!"},
		"read -format=json auth/kubernetes/role/" + beaconCIName: {out: "No value found", err: errors.New("exit status 2")},
		"write auth/kubernetes/role/" + beaconCIName + " -":      {out: "Success!"},
		// terraform: release policy and role not yet present
		"policy read " + terraformName:                            {out: "No policy named: " + terraformName, err: errors.New("exit status 2")},
		"policy write " + terraformName + " -":                    {out: "Success!"},
		"read -format=json auth/kubernetes/role/" + terraformName: {out: "No value found", err: errors.New("exit status 2")},
		"write auth/kubernetes/role/" + terraformName + " -":      {out: "Success!"},
		// terraform: CI policy and role not yet present
		"policy read " + terraformCIName:                            {out: "No policy named: " + terraformCIName, err: errors.New("exit status 2")},
		"policy write " + terraformCIName + " -":                    {out: "Success!"},
		"read -format=json auth/kubernetes/role/" + terraformCIName: {out: "No value found", err: errors.New("exit status 2")},
		"write auth/kubernetes/role/" + terraformCIName + " -":      {out: "Success!"},
	}
	runner := &fakeBaoRunner{t: t, responses: responses}
	store := openBaoExec{run: runner.run, namespace: "openbao", pod: "openbao-0"}

	items, err := ConfigurePerRepoIdentities(context.Background(), store, "root-token", identities, "oberth-pipelines")
	if err != nil {
		t.Fatalf("ConfigurePerRepoIdentities rejected producer output: %v", err)
	}

	// Each identity produces release policy + role + CI policy + role = 4 items each.
	if len(items) != 8 {
		t.Fatalf("expected 8 config items (4 per identity: release+CI policy+role), got %d: %+v", len(items), items)
	}

	// Verify that the terraform policy write included the upstream grant.
	for _, call := range runner.calls {
		if call.command == "policy write "+terraformName+" -" {
			if !strings.Contains(call.stdin, `path "oberth/data/upstream/acme/terraform/credentials"`) {
				t.Fatalf("terraform policy missing upstream grant path in written HCL:\n%s", call.stdin)
			}
			return
		}
	}
	t.Fatal("terraform policy write call not found")
}

// TestProducePerStepIdentitiesExecFailureReturnsError verifies that
// ProducePerStepIdentities returns a hard error when the kubectl exec fails,
// rather than silently returning nil. Issue #623, finding 1: a nil return
// on failure causes orphan removal to delete all per-step identities.
func TestProducePerStepIdentitiesExecFailureReturnsError(t *testing.T) {
	t.Parallel()

	failingRunner := func(_ context.Context, _ []byte, _ string, _ ...string) ([]byte, error) {
		return nil, errors.New("connection refused")
	}
	_, err := ProducePerStepIdentities(context.Background(), failingRunner, "", "oberth")
	if err == nil {
		t.Fatal("expected an error when exec fails, got nil")
	}
	if !strings.Contains(err.Error(), "exec access list --json") {
		t.Fatalf("error should mention exec access list --json: %v", err)
	}
}

// TestProducePerStepIdentitiesNilRunnerReturnsNil verifies backward compat:
// when run is nil (no exec possible, e.g. fresh install), the function returns
// nil, nil — this is safe because there are no grants to produce and no
// orphans to remove.
func TestProducePerStepIdentitiesNilRunnerReturnsNil(t *testing.T) {
	t.Parallel()

	identities, err := ProducePerStepIdentities(context.Background(), nil, "", "oberth")
	if err != nil {
		t.Fatalf("unexpected error for nil runner: %v", err)
	}
	if identities != nil {
		t.Fatalf("expected nil identities for nil runner, got %v", identities)
	}
}

// --- Issue #814: per-repo identities carry wildcard grants only ---

// TestParseAccessListJSONExcludesNamedStepGrantsFromPerRepo is the unit
// reproduction of issue #814: a wildcard and a named-step grant for one repo
// must yield a per-repo identity whose grant list is the wildcard path only.
// Before the fix the per-repo list was the union, so every template of the
// repo running under the per-repo ServiceAccount could read the named-step
// path from Vault, and revoking a wildcard grant could never narrow it.
func TestParseAccessListJSONExcludesNamedStepGrantsFromPerRepo(t *testing.T) {
	t.Parallel()
	entries := []accessListJSONEntry{
		{Repo: "codeberg/cloudtaser/cloudtaser-port", Step: "*", Secret: "oberth/data/release/cosign-secret"},
		{Repo: "codeberg/cloudtaser/cloudtaser-port", Step: "release-publish-images", Secret: "oberth/data/release/gar-image-key"},
		{Repo: "codeberg/cloudtaser/cloudtaser-port", Step: "*", Secret: "oberth/data/release/r2-upload-token"},
	}
	data, _ := json.Marshal(entries)
	result, err := ParseAccessListJSON(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(result) != 1 {
		t.Fatalf("expected 1 identity, got %d: %+v", len(result), result)
	}
	want := []string{"oberth/data/release/cosign-secret", "oberth/data/release/r2-upload-token"}
	if got := result[0].Grants; !reflect.DeepEqual(got, want) {
		t.Fatalf("per-repo grants must be the wildcard paths only (issue #814): got %v want %v", got, want)
	}
}

// TestParseAccessListJSONWithStepsSplitsPerRepoAndPerStepPaths proves the two
// path sets the per-step design (#623) promises: the per-repo identity reads
// wildcard paths only; the per-step identity reads its named paths plus the
// inherited wildcards. A named-step path never appears on the per-repo side.
func TestParseAccessListJSONWithStepsSplitsPerRepoAndPerStepPaths(t *testing.T) {
	t.Parallel()
	entries := []accessListJSONEntry{
		{Repo: "codeberg/cloudtaser/cloudtaser-port", Step: "*", Secret: "oberth/data/release/cosign-secret"},
		{Repo: "codeberg/cloudtaser/cloudtaser-port", Step: "release-publish-images", Secret: "oberth/data/release/gar-image-key"},
	}
	data, _ := json.Marshal(entries)
	repoIdentities, grants, err := ParseAccessListJSONWithSteps(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(repoIdentities) != 1 {
		t.Fatalf("expected 1 per-repo identity, got %d: %+v", len(repoIdentities), repoIdentities)
	}
	if got, want := repoIdentities[0].Grants, []string{"oberth/data/release/cosign-secret"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("per-repo grants: got %v want %v", got, want)
	}
	if len(grants) != 2 {
		t.Fatalf("expected both raw grants to be carried for per-step derivation, got %d", len(grants))
	}
	steps := DerivePerStepIdentities(repoIdentities, grants)
	if len(steps) != 1 || steps[0].Step != "release-publish-images" {
		t.Fatalf("expected one per-step identity for release-publish-images, got %+v", steps)
	}
	wantStep := []string{"oberth/data/release/cosign-secret", "oberth/data/release/gar-image-key"}
	if !reflect.DeepEqual(steps[0].GrantPaths, wantStep) {
		t.Fatalf("per-step paths must be named ∪ wildcard: got %v want %v", steps[0].GrantPaths, wantStep)
	}
}

// TestParseAccessListJSONNamedOnlyRepoKeepsIdentityWithoutGrants: a repo whose
// grants are all named still needs its per-repo ServiceAccount (workflow
// default, chart SA list, admission bookkeeping). It must be produced with an
// empty grant list rather than dropped, which would also trip the
// format-drift guard.
func TestParseAccessListJSONNamedOnlyRepoKeepsIdentityWithoutGrants(t *testing.T) {
	t.Parallel()
	entries := []accessListJSONEntry{
		{Repo: "codeberg/cloudtaser/cttv", Step: "release-publish-images", Secret: "oberth/data/release/gar-image-key"},
	}
	data, _ := json.Marshal(entries)
	result, err := ParseAccessListJSON(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(result) != 1 {
		t.Fatalf("expected the named-only repo to keep its per-repo identity, got %d: %+v", len(result), result)
	}
	if len(result[0].Grants) != 0 {
		t.Fatalf("named-only repo must have no per-repo grants, got %v", result[0].Grants)
	}
	if result[0].Repo != "cttv" || result[0].Org != "cloudtaser" || result[0].Upstream != "codeberg" {
		t.Fatalf("unexpected identity: %+v", result[0])
	}
}

// TestParseAccessListOutputExcludesNamedStepGrants covers the tabwriter
// fallback path (older deployed binaries): the STEP column decides.
func TestParseAccessListOutputExcludesNamedStepGrants(t *testing.T) {
	t.Parallel()
	output := strings.Join([]string{
		"REPO                                      STEP                     SECRET                                 APPROVED BY   APPROVED AT        STATUS",
		"codeberg/cloudtaser/cloudtaser-port       *                        oberth/data/release/cosign-secret      admin         2026-08-21 16:40   active",
		"codeberg/cloudtaser/cloudtaser-port       release-publish-images   oberth/data/release/gar-image-key      fabian        2026-10-07 18:00   active",
	}, "\n")
	result, err := ParseAccessListOutput([]byte(output))
	if err != nil {
		t.Fatal(err)
	}
	if len(result) != 1 {
		t.Fatalf("expected 1 identity, got %d: %+v", len(result), result)
	}
	if got, want := result[0].Grants, []string{"oberth/data/release/cosign-secret"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("tabwriter per-repo grants: got %v want %v", got, want)
	}
}

// TestProduceFromConfigMapExcludesNamedStepGrants covers the ConfigMap
// fallback (no running pod): the step field decides there too.
func TestProduceFromConfigMapExcludesNamedStepGrants(t *testing.T) {
	t.Parallel()
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: secretAccessConfigMapName, Namespace: "oberth"},
		Data: map[string]string{
			secretAccessConfigMapKey: `
- repo: codeberg/cloudtaser/cloudtaser-port
  step: "*"
  secret: oberth/data/release/cosign-secret
- repo: codeberg/cloudtaser/cloudtaser-port
  step: release-publish-images
  secret: oberth/data/release/gar-image-key
`,
		},
	}
	kube := fake.NewSimpleClientset(cm)
	result, _, err := ProducePerRepoIdentities(context.Background(), kube, nil, "", "oberth")
	if err != nil {
		t.Fatal(err)
	}
	if len(result) != 1 {
		t.Fatalf("expected 1 identity, got %d: %+v", len(result), result)
	}
	if got, want := result[0].Grants, []string{"oberth/data/release/cosign-secret"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ConfigMap per-repo grants: got %v want %v", got, want)
	}
}
