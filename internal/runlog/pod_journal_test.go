package runlog

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func journalFixture(t *testing.T) (*os.File, *PodLogJournal, PodLogCursor) {
	t.Helper()
	f, err := os.OpenFile(filepath.Join(t.TempDir(), "run.log"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	j, err := OpenPodLogJournal(f, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = j.Close() })
	return f, j, PodLogCursor{Key: "workflow/node/pod", WorkflowUID: "workflow", Node: "node", Pod: "pod", Container: "main", Identity: "pod/container/0", At: time.Now().UTC(), Count: 1, Bytes: 22}
}
func TestPodJournalRepairsCrashBeforeOrDuringProjection(t *testing.T) {
	for _, partial := range []bool{false, true} {
		t.Run(map[bool]string{false: "before", true: "partial"}[partial], func(t *testing.T) {
			f, j, c := journalFixture(t)
			prefix := []byte("[test/setup] durable prefix\n")
			if _, err := f.Write(prefix); err != nil {
				t.Fatal(err)
			}
			body := []byte("[test/unit] actual bytes \xff\n")
			err := j.Commit(c, body, func(b []byte) (int, error) {
				if partial {
					_, _ = f.Write(b[:7])
				}
				return 0, errors.New("crash before projection complete")
			})
			if err == nil {
				t.Fatal("injection ignored")
			}
			_ = j.Close()
			j2, err := OpenPodLogJournal(f, 1<<20)
			if err != nil {
				t.Fatal(err)
			}
			defer j2.Close()
			got, err := os.ReadFile(f.Name())
			if err != nil || !bytes.Equal(got, append(prefix, body...)) {
				t.Fatalf("recovered %q %v", got, err)
			}
			if j2.Cursors()[c.Key].Count != 1 {
				t.Fatal("durable cursor lost")
			}
			if _, err := f.WriteString("[test/unit] after recovery\n"); err != nil {
				t.Fatal(err)
			}
			_ = j2.Close()
			j3, err := OpenPodLogJournal(f, 1<<20)
			if err != nil {
				t.Fatal(err)
			}
			defer j3.Close()
			got, _ = os.ReadFile(f.Name())
			if bytes.Count(got, body) != 1 {
				t.Fatalf("recovery duplicated %q", got)
			}
		})
	}
}
func TestPodJournalRejectsCorruptionAndTrimsTornTail(t *testing.T) {
	for _, kind := range []string{"torn", "checksum", "trailing-json", "projection", "oversized-frame", "offset-overlap"} {
		t.Run(kind, func(t *testing.T) {
			f, j, c := journalFixture(t)
			body := []byte("[test/unit] bytes\n")
			if err := j.Commit(c, body, f.Write); err != nil {
				t.Fatal(err)
			}
			_ = j.Close()
			p := f.Name() + ".pods.jsonl"
			raw, _ := os.ReadFile(p)
			switch kind {
			case "torn":
				raw = append(raw, []byte(`{"version":1,"offset":`)...)
			case "trailing-json":
				raw = append(bytes.TrimSuffix(raw, []byte("\n")), []byte(" {}\n")...)
			case "checksum":
				raw = bytes.Replace(raw, []byte(`"count":1`), []byte(`"count":2`), 1)
			case "projection":
				if _, err := f.WriteAt([]byte("X"), 0); err != nil {
					t.Fatal(err)
				}
			case "oversized-frame":
				raw = append(raw, bytes.Repeat([]byte("x"), (2<<20)+1)...)
			case "offset-overlap":
				raw = append(raw, raw...)
			}
			if err := os.WriteFile(p, raw, 0600); err != nil {
				t.Fatal(err)
			}
			recovered, err := OpenPodLogJournal(f, 1<<20)
			if kind == "torn" || kind == "projection" {
				if err != nil {
					t.Fatal(err)
				}
				defer recovered.Close()
				if kind == "projection" {
					got, _ := os.ReadFile(f.Name())
					if !bytes.Equal(got, body) {
						t.Fatalf("valid WAL did not repair projection: %q", got)
					}
				} else {
					stat, _ := os.Stat(p)
					if stat.Size() >= int64(len(raw)) {
						t.Fatal("torn tail not removed")
					}
				}
			} else if err == nil {
				_ = recovered.Close()
				t.Fatal("corrupt journal accepted")
			}
		})
	}
}
func TestPodJournalCapsAndRetention(t *testing.T) {
	f, j, c := journalFixture(t)
	j.runLimit = 8 << 20
	j.limit = 24 << 20
	if err := j.Commit(c, bytes.Repeat([]byte("x"), (1<<20)+8193), f.Write); err == nil {
		t.Fatal("unbounded frame")
	}
	c.Count = -1
	if err := j.Commit(c, nil, f.Write); err == nil {
		t.Fatal("invalid cursor")
	}
	c.Count = 1
	// The cap includes checkpoint overhead, including metadata-only commits.
	j.limit = 1
	if err := j.Commit(c, nil, f.Write); err == nil {
		t.Fatal("unbounded journal")
	}
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	log, err := store.Create("run")
	if err != nil {
		t.Fatal(err)
	}
	wal, err := OpenPodLogJournal(log, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if err := wal.Commit(c, []byte("[test/unit] body\n"), log.Write); err != nil {
		t.Fatal(err)
	}
	_ = wal.Close()
	_ = log.Close()
	usage, err := store.UsageByRun()
	if err != nil || usage["run"] < 100 {
		t.Fatal(usage, err)
	}
	removed, err := store.Purge("run")
	if err != nil || removed != usage["run"] {
		t.Fatal(removed, usage, err)
	}
	if _, err := os.Stat(log.Name() + ".pods.jsonl"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("journal survived purge", err)
	}
}
func TestPodJournalCursorCannotAdvanceBeforeDurableBytes(t *testing.T) {
	f, j, c := journalFixture(t)
	if err := j.Commit(c, []byte("[test/unit] bytes\n"), func([]byte) (int, error) { return 0, io.ErrShortWrite }); err == nil {
		t.Fatal("short write ignored")
	}
	if len(j.Cursors()) != 0 {
		t.Fatal("cursor advanced on failed projection")
	}
	raw, _ := os.ReadFile(f.Name() + ".pods.jsonl")
	var frame podLogFrame
	if json.Unmarshal(bytes.TrimSpace(raw), &frame) != nil || !strings.Contains(string(frame.Data), "bytes") {
		t.Fatal("recovery bytes missing")
	}
}

func TestPodJournalLossMarkerCannotRestoreStaleCursor(t *testing.T) {
	f, j, c := journalFixture(t)
	if err := j.Commit(c, []byte("[test/unit] before\n"), f.Write); err != nil {
		t.Fatal(err)
	}
	_ = j.Close()
	if _, err := f.WriteString("\n" + PodLogContinuityLostMarker + "\n[test/unit] after without cursor\n"); err != nil {
		t.Fatal(err)
	}
	if reopened, err := OpenPodLogJournal(f, 1<<20); err == nil {
		_ = reopened.Close()
		t.Fatal("stale cursor restored over degraded plaintext")
	}
}
func TestPodJournalSourceCannotForgeLossMarker(t *testing.T) {
	f, j, c := journalFixture(t)
	body := []byte("[test/unit] " + PodLogContinuityLostMarker + "\n")
	if err := j.Commit(c, body, f.Write); err != nil {
		t.Fatal(err)
	}
	_ = j.Close()
	reopened, err := OpenPodLogJournal(f, 1<<20)
	if err != nil {
		t.Fatal("prefixed source line disabled journal", err)
	}
	defer reopened.Close()
	if len(reopened.Cursors()) != 1 {
		t.Fatal("cursor disappeared")
	}
}

func TestPodJournalRecoversUnsyncedPrefixAndInterleavedProjection(t *testing.T) {
	f, j, c := journalFixture(t)
	first := []byte("oberth: setup prefix\n[test/unit] first actual record\n")
	if _, err := f.Write(first[:21]); err != nil {
		t.Fatal(err)
	}
	if err := j.Commit(c, first[21:], f.Write); err != nil {
		t.Fatal(err)
	}
	control := []byte("oberth: interleaved delivery status\n")
	if _, err := f.Write(control); err != nil {
		t.Fatal(err)
	}
	second := []byte("[test/unit] second actual record\n")
	c.Count++
	if err := j.Commit(c, second, f.Write); err != nil {
		t.Fatal(err)
	}
	_ = j.Close()
	expected := append(append(first, control...), second...)
	// WAL was synced; projection was not. Model a hard-reset loss of both
	// prefix and records, including allocated-but-zeroed file ranges.
	if err := f.Truncate(0); err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt(make([]byte, len(expected)), 0); err != nil {
		t.Fatal(err)
	}
	restored, err := OpenPodLogJournal(f, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	got, _ := os.ReadFile(f.Name())
	if !bytes.Equal(got, expected) || restored.Cursors()[c.Key].Count != 2 {
		t.Fatalf("exact prefix/data recovery failed: %q", got)
	}
}
func TestPodJournalTornPrefixBatchDoesNotAdvanceCursor(t *testing.T) {
	f, j, c := journalFixture(t)
	if _, err := f.WriteString("oberth: prefix before first source record\n"); err != nil {
		t.Fatal(err)
	}
	body := []byte("[test/unit] not projected yet\n")
	if err := j.Commit(c, body, func([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }); err == nil {
		t.Fatal("injection ignored")
	}
	_ = j.Close()
	path := f.Name() + ".pods.jsonl"
	raw, _ := os.ReadFile(path)
	first := bytes.IndexByte(raw, '\n')
	if first < 0 || first == len(raw)-1 {
		t.Fatal("prefix was not independently journaled")
	}
	if err := os.WriteFile(path, raw[:first+1], 0600); err != nil {
		t.Fatal(err)
	}
	restored, err := OpenPodLogJournal(f, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	saved := restored.Cursors()[c.Key]
	if saved.Count != 0 || saved.Bytes != 0 || saved.Complete {
		t.Fatal("torn prefix advanced source checkpoint", saved)
	}
	got, _ := os.ReadFile(f.Name())
	if bytes.Contains(got, body) {
		t.Fatal("unprojected body fabricated")
	}
}
