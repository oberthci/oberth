package runlog

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/logcontract"
)

func usageClock() time.Time { return time.Date(2026, 11, 27, 0, 0, 0, 0, time.UTC) }

func TestLogUsageBudget(t *testing.T) {
	s := newStore(t, DefaultRunLimitBytes)
	seedUsageBudget(t, s)
	before, err := os.ReadDir(s.directory)
	if err != nil {
		t.Fatal(err)
	}
	// Inventory and index inspection complete even with the append gate held.
	if err := s.lock(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer s.unlock()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	out, err := s.Inventory(ctx, usageClock)
	if err != nil {
		t.Fatal(err)
	}
	checkBudgetByteClasses(t, out)
	checkBudgetHeadroom(t, out)
	usage, err := s.Usage(ctx, "run", usageClock())
	if err != nil || !usage.AccountingComplete || usage.Body.Logical != 64<<20 || usage.Index.Logical != 8<<20 || usage.EligibilityKnown {
		t.Fatal(usage, err)
	}
	after, err := os.ReadDir(s.directory)
	if err != nil || len(after) != len(before) {
		t.Fatal("preview changed evidence", err)
	}
}

func seedUsageBudget(t *testing.T, s *Store) {
	t.Helper()
	body, err := s.root.OpenFile("run.log", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err := body.Truncate(64 << 20); err != nil {
		t.Fatal(err)
	}
	if err := body.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(index{RunID: "run", Size: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, []byte(strings.Repeat(" ", maxIndexBytes-len(data)))...)
	if err := os.WriteFile(filepath.Join(s.directory, "run.index.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.directory, ".index-leftover"), []byte("stage"), 0600); err != nil {
		t.Fatal(err)
	}
	// Fill multiple inventory pages with real physical archive names.
	for i := 0; i < 260; i++ {
		if err := os.WriteFile(filepath.Join(s.directory, strings.Repeat("x", i%70+1)+time.Duration(i).String()+".log"), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func checkBudgetByteClasses(t *testing.T, out logcontract.StoragePreview) {
	t.Helper()
	if out.Body.Logical != 64<<20 || out.Index.Logical != 8<<20 || out.Staging.Logical != 5 || !out.Complete {
		t.Fatal(out)
	}
	if out.Body.Allocated >= out.Body.Logical || out.Index.Allocated < out.Index.Logical || out.FreeBytes == 0 || out.FreeInodes == 0 {
		t.Fatal("sparse/allocation/filesystem accounting", out)
	}
}

func checkBudgetHeadroom(t *testing.T, out logcontract.StoragePreview) {
	t.Helper()
	if out.LogicalBytes != out.Body.Logical+out.Index.Logical+out.Staging.Logical+out.Directory.Logical || out.BudgetHeadroomBytes == nil || *out.BudgetHeadroomBytes != out.BudgetBytes-max(out.LogicalBytes, out.AllocatedBytes) {
		t.Fatal("budget accounting", out)
	}
	if out.ReclaimableBytes != nil || out.RequiredReserveBytes != nil {
		t.Fatal("preview claimed unspecified purge or drain", out)
	}
}

func TestLogUsageUnknownAndNonregularRemainVisible(t *testing.T) {
	s := newStore(t, 64)
	seedUnknownUsage(t, s)
	out, err := s.Inventory(t.Context(), usageClock)
	if err != nil || out.Complete || out.UnknownEntries != 3 || out.NonregularEntries != 2 || out.Body.Logical != 0 || out.Staging.Logical != 7 || out.BudgetHeadroomBytes != nil {
		t.Fatal(out, err)
	}
	if out.Unknown.Logical < 7 || out.LogicalBytes >= 10000 {
		t.Fatal("followed link or hid unknown bytes", out)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = s.Inventory(ctx, usageClock)
	if !errors.Is(err, context.Canceled) {
		t.Fatal("inventory ignored cancellation", err)
	}
}

func seedUnknownUsage(t *testing.T, s *Store) {
	t.Helper()
	for _, name := range []string{"orphan", ".capture-leftover"} {
		if err := os.WriteFile(filepath.Join(s.directory, name), []byte("1234567"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte(strings.Repeat("s", 10000)), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(s.directory, "evil.log")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(s.directory, "nested"), 0700); err != nil {
		t.Fatal(err)
	}
}

func TestLogUsageUnlinkedSpoolAndReservation(t *testing.T) {
	s := newStore(t, 64)
	r, err := s.Reserve(t.Context(), "run", native("one"))
	if err != nil {
		t.Fatal(err)
	}
	spool, err := r.stage(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := spool.WriteString("unlinked output"); err != nil {
		t.Fatal(err)
	}
	out, err := s.Inventory(t.Context(), usageClock)
	checkOpenSpoolUsage(t, out, err)
	s.closeSpool(spool)
	if err := r.Release(t.Context()); err != nil {
		t.Fatal(err)
	}
	out, err = s.Inventory(t.Context(), usageClock)
	if err != nil || out.OpenSpoolCount != 0 || out.OpenSpools != (logcontract.StorageBytes{}) || out.ActiveReservations != 0 {
		t.Fatal("spool/reservation not released", out, err)
	}
}

func checkOpenSpoolUsage(t *testing.T, out logcontract.StoragePreview, err error) {
	t.Helper()
	if err != nil || out.OpenSpoolCount != 1 || out.ActiveReservations != 1 || out.OpenSpools.Logical != 15 || out.OpenSpools.Allocated < 15 || out.Staging.Logical != 0 {
		t.Fatal(out, err)
	}
}

func TestLogUsageCaptureAndWindowTruncation(t *testing.T) {
	s := newStore(t, 8)
	capture(t, s, native("one"), "a\nb\nc\nd\nmore")
	usage, err := s.Usage(t.Context(), "run", usageClock())
	if err != nil || !usage.CaptureLimited || usage.CaptureReason != limitReason || !usage.Incomplete || usage.Body.Logical != 8 {
		t.Fatal(usage, err)
	}
	other := newStore(t, 64)
	capture(t, other, native("one"), "a\nb\nc\n")
	_, meta, err := other.Read(t.Context(), "run", ReadRequest{Filter: Filter{Tail: true, Limit: 1}})
	if err != nil || !meta.Truncated {
		t.Fatal(meta, err)
	}
	usage, err = other.Usage(t.Context(), "run", usageClock())
	if err != nil || usage.CaptureLimited || usage.Incomplete {
		t.Fatal("window mistaken for capture loss", usage, err)
	}
}
