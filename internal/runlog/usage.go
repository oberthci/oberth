package runlog

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/oberthci/oberth/internal/logcontract"
)

func storageBytes(info os.FileInfo) (logcontract.StorageBytes, bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Blocks < 0 {
		return logcontract.StorageBytes{Logical: info.Size()}, false
	}
	return logcontract.StorageBytes{Logical: info.Size(), Allocated: stat.Blocks * 512}, true
}

func addBytes(to *logcontract.StorageBytes, value logcontract.StorageBytes) {
	to.Logical += value.Logical
	to.Allocated += value.Allocated
}

// Inventory pages directory work in batches of 128 and never takes the append
// gate. It follows no links, reads no output, and holds no database transaction.
func (s *Store) Inventory(ctx context.Context, clock func() time.Time) (logcontract.StoragePreview, error) {
	out := logcontract.StoragePreview{StartedAt: clock(), Complete: true, BudgetBytes: 6 << 30, ReviewBytes: 4 << 30, TargetBytes: 3 << 30}
	dir, err := s.root.Open(".")
	if err != nil {
		return out, storage(cannotListLogRootReason, err)
	}
	defer func() { _ = dir.Close() }()
	info, err := dir.Stat()
	if err != nil {
		return out, storage(cannotInspectLogEvidenceReason, err)
	}
	out.Directory, out.Complete = storageBytes(info)
	var stat syscall.Statfs_t
	if err := syscall.Fstatfs(int(dir.Fd()), &stat); err != nil {
		return out, storage(cannotInspectLogEvidenceReason, err)
	}
	out.FreeBytes, out.FreeInodes = stat.Bavail*uint64(stat.Bsize), stat.Ffree
	if err := s.inventoryDirectory(ctx, dir, &out); err != nil {
		return out, err
	}
	s.spoolUsage(&out)
	s.active.Range(func(_, _ any) bool { out.ActiveReservations++; return true })
	for _, size := range []logcontract.StorageBytes{out.Body, out.Index, out.Staging, out.Unknown, out.Directory, out.OpenSpools} {
		out.LogicalBytes += size.Logical
		out.AllocatedBytes += size.Allocated
	}
	out.FinishedAt = clock()
	if out.Complete {
		out.BudgetHeadroomBytes = new(max(int64(0), out.BudgetBytes-max(out.LogicalBytes, out.AllocatedBytes)))
	}
	return out, nil
}

func (s *Store) inventoryDirectory(ctx context.Context, dir *os.File, out *logcontract.StoragePreview) error {
	for {
		if err := ctx.Err(); err != nil {
			return storage(logOperationCanceledReason, err)
		}
		entries, readErr := dir.ReadDir(128)
		for _, entry := range entries {
			if err := ctx.Err(); err != nil {
				return storage(logOperationCanceledReason, err)
			}
			s.inventoryEntry(entry.Name(), out)
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return storage(cannotListLogEvidenceReason, readErr)
		}
	}
	return nil
}

func (s *Store) inventoryEntry(name string, out *logcontract.StoragePreview) {
	info, err := s.root.Lstat(name)
	if err != nil {
		out.Complete = false
		out.UnknownEntries++
		return
	}
	size, known := storageBytes(info)
	out.Complete = out.Complete && known
	if !info.Mode().IsRegular() {
		out.NonregularEntries++
		out.UnknownEntries++
		out.Complete = false
		addBytes(&out.Unknown, size)
		return
	}
	switch {
	case ownedRun(name) != "" && strings.HasSuffix(name, ".log"):
		addBytes(&out.Body, size)
	case ownedRun(name) != "":
		addBytes(&out.Index, size)
	case strings.HasPrefix(name, ".index-") || strings.HasPrefix(name, ".capture-"):
		addBytes(&out.Staging, size)
	default:
		out.UnknownEntries++
		out.Complete = false
		addBytes(&out.Unknown, size)
	}
}

// Usage reads an immutable index outside the gate; byte counts are advisory
// observations. Capture truncation is separate from Meta's selected window.
func (s *Store) Usage(ctx context.Context, id string, now time.Time) (logcontract.Usage, error) {
	out := logcontract.Usage{PhysicalRunID: id, SampledAt: now, CaptureLimitBytes: s.limit, AccountingComplete: true, ProtectionReason: "archive eligibility unknown"}
	if err := checkID(id); err != nil {
		return out, err
	}
	if err := ctx.Err(); err != nil {
		return out, storage(logOperationCanceledReason, err)
	}
	s.usageFiles(id, &out)
	saved, err := s.usageIndex(id)
	if err != nil {
		out.AccountingComplete = false
		out.Incomplete = true
		return out, nil
	}
	out.CaptureLimited = saved.Reason == limitReason
	out.Incomplete = saved.Size != out.Body.Logical
	for _, entry := range saved.Containers {
		out.CaptureLimited = out.CaptureLimited || entry.Reason == limitReason
		out.Incomplete = out.Incomplete || entry.State != Captured || entry.Reason != ""
	}
	if out.CaptureLimited {
		out.CaptureReason = limitReason
		out.Incomplete = true
	}
	return out, nil
}

func (s *Store) usageFiles(id string, out *logcontract.Usage) {
	for _, file := range []struct {
		name string
		size *logcontract.StorageBytes
	}{{id + ".log", &out.Body}, {id + ".index.json", &out.Index}} {
		info, err := s.root.Lstat(file.name)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil || !info.Mode().IsRegular() {
			out.AccountingComplete = false
			continue
		}
		var known bool
		*file.size, known = storageBytes(info)
		out.AccountingComplete = out.AccountingComplete && known
	}
}

func (s *Store) usageIndex(id string) (index, error) {
	file, err := s.root.OpenFile(id+".index.json", os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if errors.Is(err, os.ErrNotExist) {
		return index{RunID: id}, nil
	}
	if err != nil {
		return index{}, err
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return index{}, storage(invalidLogIndexFileOrSizeReason, err)
	}
	data, err := io.ReadAll(io.LimitReader(file, maxIndexBytes+1))
	if err != nil || len(data) > maxIndexBytes {
		return index{}, storage(cannotReadLogIndexReason, err)
	}
	return decodeIndex(data, id)
}
