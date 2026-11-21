package installer_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/installer"
	apps "k8s.io/api/apps/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestExistingInstallationRequiresOriginalProfile(t *testing.T) {
	t.Parallel()
	for _, changedPorts := range []bool{false, true} {
		for _, recheck := range []bool{false, true} {
			for _, key := range []string{"absent", "foreign", "missing"} {
				t.Run(key+map[bool]string{true: "/ports"}[changedPorts]+map[bool]string{true: "/recheck"}[recheck], func(t *testing.T) { requireInstallationOwner(t, ownerCase{key, changedPorts, recheck}) })
			}
		}
	}
}

type ownerCase struct {
	key                   string
	changedPorts, recheck bool
}

func requireInstallationOwner(t *testing.T, c ownerCase) {
	t.Helper()
	f := setup(t)
	installed := apply(t, f, prepare(t, f))
	if _, err := f.client.AppsV1().Deployments("oberth").Create(t.Context(), &apps.Deployment{ObjectMeta: meta.ObjectMeta{Name: "oberth", Namespace: "oberth", Generation: 1}}, meta.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if c.changedPorts {
		f.choices.HTTPSNodePort, f.choices.SSHNodePort = 31443, 31022
	}
	var plan *installer.Plan
	if c.recheck {
		plan = prepare(t, f)
	}
	dir := replaceOwnerCredentials(t, f, installed.Profile, c.key)
	before, count := installationSnapshot(t, f), len(f.charts.Applies)
	f.client.ClearActions()
	var err error
	if c.recheck {
		_, err = f.engine.Apply(t.Context(), plan, plan.Digest(), nil)
	} else {
		_, err = f.engine.Prepare(t.Context(), f.release, f.choices)
	}
	requireOwnerRefusal(t, err)
	deltaAssertOnlyClusterReads(t, f, 0)
	if !bytes.Equal(before, installationSnapshot(t, f)) || len(f.charts.Applies) != count {
		t.Fatal("release, administrator Secret or Deployment changed")
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("refusal created a profile directory", err)
	}
}

func installationSnapshot(t *testing.T, f fixture) []byte {
	t.Helper()
	secrets, err := f.client.CoreV1().Secrets("oberth").List(t.Context(), meta.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	deployments, err := f.client.AppsV1().Deployments("oberth").List(t.Context(), meta.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal([]any{f.charts.History, secrets, deployments})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestOwnedUpdateReviewsChangedAnswersAndKeepsAdministrator(t *testing.T) {
	t.Parallel()
	f := setup(t)
	apply(t, f, prepare(t, f))
	before, err := f.client.CoreV1().Secrets("oberth").Get(t.Context(), "oberth-admin", meta.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	f.choices.HTTPSNodePort, f.choices.SSHNodePort = 31443, 31022
	plan := prepare(t, f)
	review := strings.Join(plan.Review(), "\n")
	for _, expected := range []string{"Update existing oberth/oberth", "30443 -> 31443", "30022 -> 31022"} {
		if !strings.Contains(review, expected) {
			t.Fatal("update change hidden", expected, review)
		}
	}
	apply(t, f, plan)
	after, err := f.client.CoreV1().Secrets("oberth").Get(t.Context(), "oberth-admin", meta.GetOptions{})
	a, _ := json.Marshal(before)
	b, _ := json.Marshal(after)
	if err != nil || !bytes.Equal(a, b) || f.charts.History.Latest.Number != 2 {
		t.Fatal("owned update replaced the administrator", err)
	}
}

func TestForeignChartCannotBeReplacedUnderProductReleaseName(t *testing.T) {
	t.Parallel()
	port, store, _ := pendingFixture(t, false)
	record, err := store.Last("oberth")
	if err != nil {
		t.Fatal(err)
	}
	record.Chart.Metadata.Name = "another-product"
	if err := store.Update(record); err != nil {
		t.Fatal(err)
	}
	before, _ := json.Marshal(record)
	f := setup(t)
	f.engine.Charts = port
	f.client.ClearActions()
	_, err = f.engine.Prepare(t.Context(), f.release, f.choices)
	if err == nil || !strings.Contains(err.Error(), "another product") || !strings.Contains(err.Error(), "oberth/oberth") {
		t.Fatal("foreign chart accepted", err)
	}
	after, err := store.Last("oberth")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(after)
	if !bytes.Equal(before, raw) {
		t.Fatal("foreign release changed")
	}
	assertNoInstallWrites(t, f)
}

func replaceOwnerCredentials(t *testing.T, f fixture, installed installer.Profile, key string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "fresh-home", "profile")
	switch key {
	case "foreign":
		other := setup(t)
		apply(t, other, prepare(t, other))
		f.engine.Credentials = other.engine.Credentials
	case "missing":
		if err := os.Remove(installed.AdminKey); err != nil {
			t.Fatal(err)
		}
	default:
		credentials, err := installer.OpenConfigDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = credentials.Close() })
		f.engine.Credentials = credentials
	}
	return dir
}

func requireOwnerRefusal(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("another profile took over the installation")
	}
	for _, word := range []string{"oberth/oberth", "profile", "--namespace", "--https-node-port", "--ssh-node-port"} {
		if !strings.Contains(err.Error(), word) {
			t.Fatal("takeover refusal omits recovery choice", word, err)
		}
	}
}

func TestInstallationAppearingAtRevalidationDoesNotCreateLocalDirectory(t *testing.T) {
	t.Parallel()
	f := setup(t)
	plan := prepare(t, f)
	fresh := setup(t)
	fresh.engine.Cluster = f.engine.Cluster
	fresh.engine.Charts = f.engine.Charts
	freshPlan := prepare(t, fresh)
	directory := filepath.Join(t.TempDir(), "absent", "profile")
	credentials, err := installer.OpenConfigDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer credentials.Close()
	f.engine.Credentials = credentials
	_, err = f.engine.Apply(t.Context(), plan, plan.Digest(), func(phase installer.Phase) error {
		if phase == installer.Checking {
			apply(t, fresh, freshPlan)
		}
		return nil
	})
	requireOwnerRefusal(t, err)
	if _, err := os.Stat(directory); !os.IsNotExist(err) {
		t.Fatal("revalidation created a local directory", err)
	}
}
