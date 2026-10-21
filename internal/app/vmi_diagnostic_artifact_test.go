package app

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"testing"

	"github.com/oberthci/oberth/pkg/periapsis"
)

func diagnosticArchive(t *testing.T, name, body string) []byte {
	t.Helper()
	var buffer bytes.Buffer
	gz := gzip.NewWriter(&buffer)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0600, Size: int64(len(body))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(body)); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}
func TestVMIDiagnosticArtifactPreservesCollectionAndTierGate(t *testing.T) {
	collector := &fakeCollector{body: diagnosticArchive(t, "report.txt", "test report")}
	store := &fakeArtifactStore{}
	jobs := jobsWithArtifacts(collector, store)
	diagnostic := []byte(`{"version":1,"vmis":[]}`)
	if reason := jobs.collectArtifacts(context.Background(), "wf", "run-1", periapsis.TriggerCI, diagnostic); reason != "" {
		t.Fatal(reason)
	}
	gz, err := gzip.NewReader(bytes.NewReader(store.stored))
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	reader := tar.NewReader(gz)
	members := map[string]string{}
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(reader)
		if err != nil {
			t.Fatal(err)
		}
		members[header.Name] = string(body)
	}
	if store.calls != 1 || members["report.txt"] != "test report" || members["oberth/vmi-diagnostics.json"] != string(diagnostic) {
		t.Fatalf("members=%v calls=%d", members, store.calls)
	}
	if reason := jobs.collectArtifacts(context.Background(), "wf", "run-1", periapsis.TriggerRelease, diagnostic); reason != "" || store.calls != 1 || collector.calls != 1 {
		t.Fatal("credentialed diagnostic escaped tier gate")
	}
	collector.err = errors.New("claim gone")
	if reason := jobs.collectArtifacts(context.Background(), "wf", "run-2", periapsis.TriggerCI, diagnostic); reason == "" || store.calls != 2 || len(store.stored) == 0 {
		t.Fatal("diagnostic lost with failed user artifact collection")
	}
}
func TestVMIDiagnosticArtifactRefusesCollisionAndSize(t *testing.T) {
	for _, archive := range [][]byte{diagnosticArchive(t, "oberth/vmi-diagnostics.json", "spoofed"), diagnosticArchive(t, "report.txt", "too much")} {
		if _, err := appendVMIDiagnosticArtifact(archive, []byte("{}"), 8); err == nil {
			t.Fatal("collision/overflow accepted")
		}
	}
}
