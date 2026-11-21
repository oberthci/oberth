package installer_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"reflect"
	"testing"

	"github.com/oberthci/oberth/internal/installer"
	"github.com/oberthci/oberth/internal/releaseverify"
)

func TestCanonicalReleaseMetadataBounds(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"release.json", "SHA256SUMS.sigstore.json"} {
		for _, n := range []int{releaseverify.MaxMetadata, releaseverify.MaxMetadata + 1} {
			f := releaseFixture(t)
			f.files[name].Data = append(f.files[name].Data, bytes.Repeat([]byte(" "), n-len(f.files[name].Data))...)
			if name == "release.json" {
				signRelease(t, f.files)
			}
			_, err := installer.LoadRelease(t.Context(), f.files, f.version)
			if (err == nil) != (n == releaseverify.MaxMetadata) {
				t.Fatal(name, n, err)
			}
			if n > releaseverify.MaxMetadata && !errors.Is(err, releaseverify.Limit) {
				t.Fatal("native verifier bound lost", err)
			}
		}
	}
}
func TestCanonicalReleaseArchiveBounds(t *testing.T) {
	t.Parallel()
	for _, n := range []int{64 << 20, 64<<20 + 1} {
		f := releaseFixture(t)
		f.files["oberth.tgz"].Data = append(f.files["oberth.tgz"].Data, make([]byte, n-len(f.files["oberth.tgz"].Data))...)
		signRelease(t, f.files)
		_, err := installer.LoadRelease(t.Context(), f.files, f.version)
		if (err == nil) != (n == 64<<20) {
			t.Fatal(n, err)
		}
		if n > 64<<20 && !errors.Is(err, releaseverify.Limit) {
			t.Fatal("native chart bound lost", err)
		}
	}
}
func TestCanonicalReleaseRequiresExactRequestedVersion(t *testing.T) {
	t.Parallel()
	f := releaseFixture(t)
	if _, err := installer.LoadRelease(t.Context(), f.files, "v0.0.9"); !errors.Is(err, releaseverify.Version) {
		t.Fatal("wrong requested version accepted", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := installer.LoadRelease(ctx, f.files, f.version); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled release read accepted", err)
	}
}
func TestRollbackAuthenticatesEarlierArtifactBeforeAnyWrite(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"tampered chart", "tampered signature", "wrong version", "wrong schema", "wrong protocol"} {
		t.Run(kind, func(t *testing.T) { rollbackReleaseRefusal(t, kind) })
	}
}
func rollbackReleaseRefusal(t *testing.T, kind string) {
	t.Helper()
	f, m := maintenanceFixture(t)
	schema := 1
	if kind == "wrong protocol" {
		schema = 1
	}
	f.release = loadFixture(t, releaseFor(t, "v0.1.0", schema))
	plan, err := m.PlanUpgrade(t.Context(), f.release, f.choices, installer.UpgradeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer plan.Plan.Close()
	if _, err = m.Upgrade(t.Context(), plan, plan.Plan.Digest(), nil); err != nil {
		t.Fatal(err)
	}
	earlier := releaseFor(t, "v0.0.9", 1)
	earlier = refusedEarlier(t, earlier, kind)
	if kind == "wrong protocol" {
		changeRollbackRecord(t, f, kind)
	}
	m.ReleaseFiles = func(ctx context.Context, version string) (fs.FS, error) {
		if version != "v0.0.9" {
			t.Fatal("rollback selected another release", version)
		}
		return earlier.files, ctx.Err()
	}
	before, err := os.ReadFile(f.dir + "/rollback.json")
	if err != nil {
		t.Fatal(err)
	}
	snapshot := rollbackSnapshot{Record: before, History: f.charts.History, Applies: len(f.charts.Applies)}
	if err = m.Rollback(t.Context()); err == nil {
		t.Fatal("untrusted rollback accepted")
	}
	assertRollbackUnchanged(t, f, snapshot)

}
func changeRollbackRecord(t *testing.T, f fixture, kind string) {
	t.Helper()
	path := f.dir + "/rollback.json"
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var record installer.Record
	if err = json.Unmarshal(body, &record); err != nil {
		t.Fatal(err)
	}
	if kind == "wrong protocol" {
		record.Options.Protocol.AuditDomain = "other"
	} else {
		t.Fatal("unknown rollback record corruption", kind)
	}
	body, err = json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, body, 0600); err != nil {
		t.Fatal(err)
	}
}

func refusedEarlier(t *testing.T, earlier releaseData, kind string) releaseData {
	t.Helper()
	switch kind {
	case "tampered chart":
		earlier.files["oberth.tgz"].Data[9] ^= 1
	case "tampered signature":
		earlier.files["SHA256SUMS.sigstore.json"].Data[0] ^= 1
	case "wrong version":
		return releaseFixture(t)
	case "wrong schema":
		return releaseFor(t, "v0.0.9", 2)
	}
	return earlier
}

type rollbackSnapshot struct {
	Record  []byte
	History installer.History
	Applies int
}

func assertRollbackUnchanged(t *testing.T, f fixture, before rollbackSnapshot) {
	t.Helper()
	after, err := os.ReadFile(f.dir + "/rollback.json")
	if err != nil || !bytes.Equal(before.Record, after) || !reflect.DeepEqual(before.History, f.charts.History) || len(f.charts.Applies) != before.Applies {
		t.Fatal("rollback refusal changed state", err)
	}
}

func TestRollbackRefusesRevisionChangedDuringReleaseFetch(t *testing.T) {
	t.Parallel()
	f, m := maintenanceFixture(t)
	f.release = loadFixture(t, releaseFor(t, "v0.1.0", 1))
	p, err := m.PlanUpgrade(t.Context(), f.release, f.choices, installer.UpgradeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Plan.Close()
	if _, err = m.Upgrade(t.Context(), p, p.Plan.Digest(), nil); err != nil {
		t.Fatal(err)
	}
	earlier := releaseFor(t, "v0.0.9", 1)
	before, err := os.ReadFile(f.dir + "/rollback.json")
	if err != nil {
		t.Fatal(err)
	}
	snapshot := rollbackSnapshot{Record: before, Applies: len(f.charts.Applies)}
	m.ReleaseFiles = func(ctx context.Context, _ string) (fs.FS, error) {
		f.charts.History.Latest.Number += 7
		snapshot.History = f.charts.History
		return earlier.files, ctx.Err()
	}
	if err = m.Rollback(t.Context()); err == nil {
		t.Fatal("revision changed during release fetch was overwritten")
	}
	assertRollbackUnchanged(t, f, snapshot)
}
