package installer_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/installer"
	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/release"
	"helm.sh/helm/v3/pkg/storage"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func TestHistoryDeletionRequiresNamespaceAndLiveReleaseOwnership(t *testing.T) {
	t.Parallel()
	for _, shape := range []string{"owned server", "owned pipeline", "owned cluster", "foreign namespace", "manager", "release", "release namespace"} {
		t.Run(shape, func(t *testing.T) { deletionAuthorityCase(t, shape) })
	}
}
func deletionAuthorityCase(t *testing.T, shape string) {
	t.Helper()
	store, request := reviewHistoryFixture(t)
	request.Revision, request.Recovery = 3, nil
	request.RollbackID = "reviewed"
	old, err := store.Get("oberth", 2)
	if err != nil {
		t.Fatal(err)
	}
	poisonHistoryTarget(old, "both")
	if err = store.Update(old); err != nil {
		t.Fatal(err)
	}
	request.Chart = installer.ReleaseChartForTest(loadFixture(t, releaseFixture(t)))
	kc := deltaDeleteClient(t)
	kc.live = metav1.ObjectMeta{Labels: map[string]string{"app.kubernetes.io/managed-by": "Helm"}, Annotations: map[string]string{"meta.helm.sh/release-name": "oberth", "meta.helm.sh/release-namespace": "oberth"}}
	configureDeletionCandidate(kc, shape)
	caps := chartutil.DefaultCapabilities.Copy()
	caps.KubeVersion = chartutil.KubeVersion{Version: "v1.30.0", Major: "1", Minor: "30"}
	port := installer.ChartForTest(&action.Configuration{Releases: store, KubeClient: kc, Capabilities: caps, Log: func(string, ...any) {}})
	bindChartHistory(t, port, &request)
	before := deltaHistoryBytes(t, store)
	err = port.Apply(t.Context(), request)
	if strings.HasPrefix(shape, "owned") {
		if err != nil || len(kc.deleted) != 1 || kc.updates != 1 {
			t.Fatal("owned obsolete object could not be removed", err, kc.deleted)
		}
	} else {
		var refusal *installer.Problem
		if !errors.As(err, &refusal) || refusal.Code != "history-delete" || len(kc.deleted) != 0 || kc.updates != 0 || !bytes.Equal(before, deltaHistoryBytes(t, store)) {
			t.Fatal("foreign deletion changed native state", err)
		}
	}
}

func TestNativeReleaseProvenanceIdentifiesForeignRevisions(t *testing.T) {
	t.Parallel()
	for _, shape := range []string{"missing kind", "unknown kind", "unknown older", "missing binding", "replayed binding", "changed version", "earlier verified", "changed after preview"} {
		t.Run(shape, func(t *testing.T) { provenanceRecordCase(t, shape) })
	}
}
func provenanceRecordCase(t *testing.T, shape string) {
	t.Helper()
	port, store, request := pendingFixture(t, true)
	// A local latest revision must not hide the preceding verified revisions.
	if shape == "earlier verified" {
		request.Local = true
	}
	if shape == "changed after preview" {
		if err := port.Preview(t.Context(), request); err != nil {
			t.Fatal(err)
		}
	}
	last, err := store.Last("oberth")
	if err != nil {
		t.Fatal(err)
	}
	if shape == "unknown older" {
		older, getErr := store.Get("oberth", 1)
		if getErr != nil {
			t.Fatal(getErr)
		}
		older.Labels["oberth.ci/release-kind"] = "other"
		older.Labels["oberth.ci/kind-revision"] = installer.RevisionBindingForTest(older.Version, "other", older.Chart.Metadata.AppVersion)
		if updateErr := store.Update(older); updateErr != nil {
			t.Fatal(updateErr)
		}
	}
	corruptProvenance(last, shape)
	if err = store.Update(last); err != nil {
		t.Fatal(err)
	}
	before := deltaHistoryBytes(t, store)
	assertProvenanceHistory(t, port, shape)
	if !bytes.Equal(before, deltaHistoryBytes(t, store)) {
		t.Fatal("history classification changed release history")
	}
}
func assertProvenanceHistory(t *testing.T, port installer.ChartLibrary, shape string) {
	t.Helper()
	history, err := port.Read(t.Context(), "oberth")
	if shape == "earlier verified" || shape == "changed version" {
		if err == nil {
			t.Fatal("inconsistent installer history accepted")
		}
	} else if err != nil || len(history.Foreign) != 1 {
		t.Fatal("unbound revision was not identified as foreign", history, err)
	}
}
func TestInstallSameVersionIgnoresEditableProfileFacts(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"", "local", "verified", "other"} {
		t.Run(fmt.Sprintf("kind-%s", kind), func(t *testing.T) {
			f := setup(t)
			f.release = localVersion(t, "dev", 2)
			apply(t, f, prepare(t, f))
			setInstalledMetadata(t, f, kind, 999)
			deltaWriteProfile(t, f, bytes.Replace(deltaReadProfile(t, f), []byte(`"server_version":"dev"`), []byte(`"server_version":"old"`), 1))
			result := apply(t, f, prepare(t, f))
			raw, err := json.Marshal(result.Profile)
			if err != nil || bytes.Contains(raw, []byte("release_kind")) || bytes.Contains(raw, []byte("server_schema")) {
				t.Fatal("profile still owns release facts", err)
			}
		})
	}
}
func TestInstallVersionChangeRequiresUpgradeForEveryHistoryState(t *testing.T) {
	t.Parallel()
	for _, state := range []string{"deployed", "failed", "pending-install", "pending-upgrade", "pending-rollback"} {
		t.Run(state, func(t *testing.T) {
			f := setup(t)
			f.release = localVersion(t, "dev", 2)
			apply(t, f, prepare(t, f))
			f.charts.History.Latest.Status = state
			f.release = localVersion(t, "different", 2)
			before, count := deltaReadProfile(t, f), len(f.charts.Previews)
			p, err := f.engine.Prepare(t.Context(), f.release, f.choices)
			if p != nil {
				p.Close()
			}
			var refusal *installer.Problem
			if !errors.As(err, &refusal) || refusal.Code != "upgrade-required" || !strings.Contains(refusal.Remedy, "oberth upgrade --version") || len(f.charts.Previews) != count || !bytes.Equal(before, deltaReadProfile(t, f)) {
				t.Fatal("install changed versions or refused too late", err)
			}
		})
	}
}
func TestInstallRechecksClusterKindBeforeApply(t *testing.T) {
	t.Parallel()
	f := setup(t)
	apply(t, f, prepare(t, f))
	plan := prepare(t, f)
	f.charts.History.Latest.Kind = "verified"
	f.charts.History.Verified = true
	before := len(f.client.Actions())
	_, err := f.engine.Apply(t.Context(), plan, plan.Digest(), nil)
	var refusal *installer.Problem
	if !errors.As(err, &refusal) || refusal.Code != "target" {
		t.Fatal("reviewed local eligibility was not rechecked before writes", err)
	}
	deltaAssertOnlyClusterReads(t, f, before)
	if len(f.charts.Applies) != 1 {
		t.Fatal("provenance change reached apply")
	}
}

func TestFailedFirstInstallWithoutProfileRefusesEvenSameVersion(t *testing.T) {
	t.Parallel()
	for _, verified := range []bool{false, true} {
		t.Run(boolText(verified), func(t *testing.T) {
			f := setup(t)
			if verified {
				f.release = loadFixture(t, releaseFixture(t))
			}
			f.engine.Charts = interruptedChart{ChartPort: f.charts, history: &f.charts.History}
			plan := prepare(t, f)
			if _, err := f.engine.Apply(t.Context(), plan, plan.Digest(), nil); !errors.Is(err, context.Canceled) {
				t.Fatal("fixture did not interrupt", err)
			}
			f.engine.Charts = f.charts
			if err := os.Remove(f.dir + "/installation.json"); err != nil {
				t.Fatal(err)
			}
			if _, err := f.engine.Prepare(t.Context(), f.release, f.choices); err == nil {
				t.Fatal("lost profile took over failed installation")
			}
			if len(f.charts.Applies) != 0 {
				t.Fatal("unowned recovery was applied")
			}
		})
	}
}

func configureDeletionCandidate(kc *deltaDeletionKube, shape string) {
	switch shape {
	case "owned pipeline":
		kc.info.Namespace = "oberth-runs"
	case "owned cluster":
		kc.info.Namespace = ""
		kc.info.Object = &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: kc.info.Name}}
		kc.info.Mapping.Resource = schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}
		kc.info.Mapping.GroupVersionKind.Kind = "Namespace"
		kc.info.Mapping.Scope = meta.RESTScopeRoot
	case "foreign namespace":
		kc.info.Namespace = "other"
	case "manager":
		delete(kc.live.Labels, "app.kubernetes.io/managed-by")
	case "release":
		kc.live.Annotations["meta.helm.sh/release-name"] = "another"
	case "release namespace":
		kc.live.Annotations["meta.helm.sh/release-namespace"] = "another"
	}
}

func corruptProvenance(last *release.Release, shape string) {
	switch shape {
	case "missing kind", "changed after preview":
		delete(last.Labels, "oberth.ci/release-kind")
	case "unknown kind":
		last.Labels["oberth.ci/release-kind"] = "other"
	case "missing binding":
		delete(last.Labels, "oberth.ci/kind-revision")
	case "replayed binding":
		last.Labels["oberth.ci/kind-revision"] = "2"
	case "changed version":
		last.Chart.Metadata.AppVersion = "tampered"
	case "earlier verified":
		last.Labels = installer.RevisionLabelsForTest(installer.ChartRequest{Chart: last.Chart, Local: true, Revision: last.Version - 1})
	}
}

func TestNativeUpdateRechecksDeletionOwnership(t *testing.T) {
	t.Parallel()
	store, request := reviewHistoryFixture(t)
	old, err := store.Get("oberth", 2)
	if err != nil {
		t.Fatal(err)
	}
	poisonHistoryTarget(old, "both")
	if err = store.Update(old); err != nil {
		t.Fatal(err)
	}
	request.Chart = installer.ReleaseChartForTest(loadFixture(t, releaseFixture(t)))
	kc := deltaDeleteClient(t)
	kc.live = metav1.ObjectMeta{Labels: map[string]string{"app.kubernetes.io/managed-by": "Helm"}, Annotations: map[string]string{"meta.helm.sh/release-name": "oberth", "meta.helm.sh/release-namespace": "oberth"}}
	kc.alterOwnership = true
	caps := chartutil.DefaultCapabilities.Copy()
	caps.KubeVersion = chartutil.KubeVersion{Version: "v1.30.0", Major: "1", Minor: "30"}
	port := installer.ChartForTest(&action.Configuration{Releases: store, KubeClient: kc, Capabilities: caps, Log: func(string, ...any) {}})
	bindChartHistory(t, port, &request)
	err = port.Apply(t.Context(), request)
	var refusal *installer.Problem
	if !errors.As(err, &refusal) || refusal.Code != "history-delete" || kc.gets != 2 || len(kc.deleted) != 0 || kc.updates != 0 {
		t.Fatal("changed ownership reached native resource update", err)
	}
}

func TestNativeApplyRefusesARevisionCreatedAfterPreflight(t *testing.T) {
	t.Parallel()
	store, request := reviewHistoryFixture(t)
	// Use a normal deployed predecessor for a local-to-verified crossing race.
	if _, err := store.Delete("oberth", 3); err != nil {
		t.Fatal(err)
	}
	request.Revision, request.Recovery, request.Local = 2, nil, true
	markHistoryLocal(t, store)
	kc := deltaDeleteClient(t)
	kc.onReach = func() {
		// Initial history, copied-history validation and read-only render precede
		// the actual upgrade's reachability check.
		if kc.reaches != 4 {
			return
		}
		advanced, err := store.Get("oberth", 2)
		if err != nil {
			t.Fatal(err)
		}
		advanced.Version = 3
		advanced.Labels = installer.RevisionLabelsForTest(installer.ChartRequest{Chart: advanced.Chart, Revision: 2})
		if err = store.Create(advanced); err != nil {
			t.Fatal(err)
		}
	}
	caps := chartutil.DefaultCapabilities.Copy()
	port := installer.ChartForTest(&action.Configuration{Releases: store, KubeClient: kc, Capabilities: caps, Log: func(string, ...any) {}})
	err := port.Apply(t.Context(), request)
	if kc.reaches < 4 {
		t.Fatal("race did not advance history")
	}
	if err == nil || kc.updates != 0 {
		t.Fatal("local apply crossed a concurrently verified revision", err)
	}
	last, err := store.Last("oberth")
	if err != nil || last.Version != 3 || last.Labels["oberth.ci/release-kind"] != "verified" {
		t.Fatal("revision fence wrote history", err)
	}
}

func markHistoryLocal(t *testing.T, store *storage.Storage) {
	t.Helper()
	for number := 1; number <= 2; number++ {
		r, err := store.Get("oberth", number)
		if err != nil {
			t.Fatal(err)
		}
		r.Labels = installer.RevisionLabelsForTest(installer.ChartRequest{Chart: r.Chart, Local: true, Revision: number - 1})
		if err = store.Update(r); err != nil {
			t.Fatal(err)
		}
	}
}
